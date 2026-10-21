package nupath

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func newHost(t *testing.T, sockets ...net.PacketConn) (*host.Host, *wire.NodeKey) {
	t.Helper()
	key, err := wire.GenerateNodeKey()
	check(t, err)
	h, err := host.New(host.Config{Key: key, PacketConns: sockets})
	check(t, err)
	t.Cleanup(func() { _ = h.Close() })
	return h, key
}

// A real Nu QUIC circuit-v2 relay, not an in-memory reliable-channel surrogate.
func circuitFactory(t *testing.T, ctx context.Context, sockets ...net.PacketConn) func() (io.ReadWriteCloser, io.ReadWriteCloser) {
	t.Helper()
	var groups [3][]net.PacketConn
	for i, socket := range sockets {
		groups[i] = []net.PacketConn{socket}
	}
	rh, rk := newHost(t, groups[0]...)
	ah, _ := newHost(t, groups[1]...)
	bh, _ := newHost(t, groups[2]...)
	srv, err := circuit.NewServer(rh, rk, circuit.ServerConfig{Admitted: []wire.PeerID{ah.ID(), bh.ID()}})
	check(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	ac, err := circuit.NewClient(ah, circuit.ClientConfig{})
	check(t, err)
	t.Cleanup(func() { _ = ac.Close() })
	bc, err := circuit.NewClient(bh, circuit.ClientConfig{})
	check(t, err)
	t.Cleanup(func() { _ = bc.Close() })
	relayAddr, err := wire.ParseMultiaddr(rh.Addrs()[0].String()+"/p2p/"+rh.ID().String(), 1024)
	check(t, err)
	_, err = ah.Dial(ctx, relayAddr)
	check(t, err)
	_, err = bh.Dial(ctx, relayAddr)
	check(t, err)
	_, err = bc.Reserve(ctx, rh.ID())
	check(t, err)
	opened := false
	return func() (io.ReadWriteCloser, io.ReadWriteCloser) {
		if opened {
			// Reattachment must not rely on retaining either outer QUIC connection.
			ah.Disconnect(rh.ID())
			bh.Disconnect(rh.ID())
			rh.Disconnect(ah.ID())
			rh.Disconnect(bh.ID())
			check(t, waitUntil(ctx, func() bool {
				_, aLive := rh.Connection(ah.ID())
				_, bLive := rh.Connection(bh.ID())
				return !aLive && !bLive
			}))
			_, err = ah.Dial(ctx, relayAddr)
			check(t, err)
			_, err = bh.Dial(ctx, relayAddr)
			check(t, err)
			_, err = bc.Reserve(ctx, rh.ID())
			check(t, err)
		}
		opened = true
		a, err := ac.Connect(ctx, rh.ID(), bh.ID())
		check(t, err)
		b, err := bc.Accept(ctx)
		check(t, err)
		negotiated := make(chan error, 1)
		go func() {
			_, err := wire.Negotiate(b, func(p string) bool { return p == "/nu/qpath/1" })
			negotiated <- err
		}()
		selectA, err := wire.NewLazySelect(a, "/nu/qpath/1")
		check(t, err)
		check(t, selectA.Confirm())
		check(t, <-negotiated)
		return a, b
	}
}

func tlsConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := wire.GenerateNodeKey()
	check(t, err)
	cert, err := wire.NewTLSCertificate(key, time.Now())
	check(t, err)
	return &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{wire.TLSALPN}, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAnyClientCert, InsecureSkipVerify: true, // libp2p verifier, not WebPKI
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			_, err := wire.VerifyTLSCertificate(raw, "", time.Now(), 16<<10)
			return err
		}}
}

type trace struct {
	mu      sync.Mutex
	cwnd    int
	rtt     time.Duration
	lost    int
	updates int
}

func (tr *trace) AddProducer() qlogwriter.Recorder { return tr }
func (tr *trace) SupportsSchemas(s string) bool    { return s == qlog.EventSchema }
func (tr *trace) Close() error                     { return nil }
func (tr *trace) RecordEvent(e qlogwriter.Event) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	switch ev := e.(type) {
	case qlog.MetricsUpdated:
		if ev.CongestionWindow != 0 {
			tr.cwnd = ev.CongestionWindow
		}
		if ev.SmoothedRTT != 0 {
			tr.rtt = ev.SmoothedRTT
		}
		tr.updates++
	default:
		if e.Name() == "recovery:packet_lost" || e.Name() == "quic:packet_lost" {
			tr.lost++
		}
	}
}
func (tr *trace) snapshot() string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return fmt.Sprintf("cwnd=%d srtt=%s lost=%d updates=%d", tr.cwnd, tr.rtt, tr.lost, tr.updates)
}

type pair struct {
	a, b           *packetConn
	client, server *quic.Conn
	trace          *trace
}

func connect(t *testing.T, ctx context.Context, a, b *packetConn, fixed bool) *pair {
	t.Helper()
	tr := &trace{}
	cfg := &quic.Config{Versions: []quic.Version{quic.Version1}, MaxIdleTimeout: 2 * time.Second, HandshakeIdleTimeout: 2 * time.Second, MaxIncomingUniStreams: -1, InitialStreamReceiveWindow: 64 << 10, MaxStreamReceiveWindow: 1 << 20, MaxConnectionReceiveWindow: 2 << 20}
	if fixed {
		cfg.InitialPacketSize = 1200
		cfg.DisablePathMTUDiscovery = true
	}
	at := &quic.Transport{Conn: a}
	bt := &quic.Transport{Conn: b}
	t.Cleanup(func() { _ = a.Close(); _ = b.Close(); _ = at.Close(); _ = bt.Close() })
	listener, err := bt.Listen(tlsConfig(t), cfg)
	check(t, err)
	cfg = cfg.Clone()
	cfg.Tracer = func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace { return tr }
	client, err := at.Dial(ctx, virtual, tlsConfig(t), cfg)
	check(t, err)
	server, err := listener.Accept(ctx)
	check(t, err)
	t.Cleanup(func() { _ = client.CloseWithError(0, ""); _ = server.CloseWithError(0, "") })
	// Exercise stock non-QUIC demux while QUIC is alive, on the same inner socket.
	probes := make(chan error, 1)
	go func() {
		buf := make([]byte, 256)
		n, addr, err := bt.ReadNonQUICPacket(ctx, buf)
		if err == nil && (!bytes.Equal(buf[:n], []byte("\x01disco")) || addr.String() != b.peer.String()) {
			err = fmt.Errorf("wrong probe %q from %v", buf[:n], addr)
		}
		probes <- err
	}()
	// ReadNonQUICPacket activates demux lazily. Retry bounded probes until it starts.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := at.WriteTo([]byte("\x01disco"), a.peer)
		check(t, err)
		select {
		case err := <-probes:
			check(t, err)
			return &pair{a, b, client, server, tr}
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

// Continuous deterministic 32 KiB records, paced so every scripted transition
// occurs during one active stream. Hash comparison proves no application resume.
type transfer struct {
	received atomic.Int64
	done     chan error
	stop     chan struct{}
	once     sync.Once
}

func startTransfer(ctx context.Context, p *pair) *transfer {
	x := &transfer{done: make(chan error, 1), stop: make(chan struct{})}
	block := make([]byte, 32<<10)
	for i := range block {
		block[i] = byte(i*31 + 7)
	}
	go func() {
		stream, err := p.client.OpenStreamSync(ctx)
		if err != nil {
			x.done <- err
			return
		}
		writeDone := make(chan error, 1)
		go func() {
			for {
				select {
				case <-x.stop:
					writeDone <- stream.Close()
					return
				case <-ctx.Done():
					stream.CancelWrite(1)
					writeDone <- ctx.Err()
					return
				default:
				}
				if _, err := stream.Write(block); err != nil {
					writeDone <- err
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		inbound, err := p.server.AcceptStream(ctx)
		if err != nil {
			x.done <- err
			return
		}
		got := sha256.New()
		buf := make([]byte, len(block))
		var total int64
		for {
			n, err := inbound.Read(buf)
			if n > 0 {
				_, _ = got.Write(buf[:n])
				total += int64(n)
				x.received.Store(total)
			}
			if err == io.EOF {
				err = nil
				break
			}
			if err != nil {
				x.done <- err
				return
			}
		}
		if err := <-writeDone; err != nil {
			x.done <- err
			return
		}
		want := sha256.New()
		for n := int64(0); n < total; n += int64(len(block)) {
			count := min(int64(len(block)), total-n)
			_, _ = want.Write(block[:count])
		}
		if !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
			x.done <- fmt.Errorf("payload hash mismatch")
			return
		}
		x.done <- nil
	}()
	return x
}
func (x *transfer) finish() { x.once.Do(func() { close(x.stop) }) }
func progress(t *testing.T, ctx context.Context, x *transfer, bytes int64) {
	t.Helper()
	goal := x.received.Load() + bytes
	check(t, waitUntil(ctx, func() bool { return x.received.Load() >= goal }))
}
func makePair(t *testing.T) (*packetConn, *packetConn) {
	t.Helper()
	a, err := newPacketConn("127.0.0.1:0")
	check(t, err)
	b, err := newPacketConn("127.0.0.1:0")
	check(t, err)
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	a.peer = b.LocalAddr().(*net.UDPAddr)
	b.peer = a.LocalAddr().(*net.UDPAddr)
	return a, b
}

func TestPaths(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	beforeG := runtime.NumGoroutine()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	a, b := makePair(t)
	factory := circuitFactory(t, ctx)
	ra, rb := factory()
	a.attach(ra)
	b.attach(rb)
	begin := time.Now()
	p := connect(t, ctx, a, b, true)
	x := startTransfer(ctx, p)
	defer x.finish()
	progress(t, ctx, x, 4<<20)
	t.Logf("relay first 4MiB=%s %s", time.Since(begin), p.trace.snapshot())
	a.direct.Store(true)
	b.direct.Store(true)
	switched := time.Now()
	progress(t, ctx, x, 8<<20)
	t.Logf("direct 8MiB=%s %s", time.Since(switched), p.trace.snapshot())
	// 60ms RTT with deterministic 2%% outgoing packet loss, both directions.
	a.delay.Store(int64(30 * time.Millisecond))
	b.delay.Store(int64(30 * time.Millisecond))
	a.lossEvery.Store(50)
	b.lossEvery.Store(50)
	switched = time.Now()
	progress(t, ctx, x, 1<<20)
	t.Logf("60ms RTT/2%% loss 1MiB=%s %s", time.Since(switched), p.trace.snapshot())
	// Simulate all paths lost and discard the old reliable stream, then attach a
	// fresh circuit under the SAME inner connection and stream after 250ms.
	a.blackhole.Store(true)
	b.blackhole.Store(true)
	a.detach()
	b.detach()
	switched = time.Now()
	time.Sleep(250 * time.Millisecond)
	ra, rb = factory()
	a.attach(ra)
	b.attach(rb)
	a.direct.Store(false)
	b.direct.Store(false)
	a.blackhole.Store(false)
	b.blackhole.Store(false)
	progress(t, ctx, x, 4<<20)
	t.Logf("outage + relay reattach 4MiB=%s %s", time.Since(switched), p.trace.snapshot())
	x.finish()
	select {
	case err := <-x.done:
		check(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if a.directPackets.Load() == 0 || a.relayPackets.Load() == 0 {
		t.Fatal("path did not carry packets")
	}
	if p.client.Context().Err() != nil {
		t.Fatal("inner connection replaced/closed")
	}
	t.Logf("same connection/stream verified bytes=%d elapsed=%s max_packet=%d queue_high_water=%d/256 drops=%d heap_delta=%d total_alloc_delta=%d goroutines_delta=%d", x.received.Load(), time.Since(begin), a.maxSize.Load(), a.maxQueue.Load(), a.dropped.Load(), int64(after.HeapAlloc)-int64(before.HeapAlloc), after.TotalAlloc-before.TotalAlloc, runtime.NumGoroutine()-beforeG)
}

func TestMTU(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("fixed1200=%v", fixed), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			a, b := makePair(t)
			a.direct.Store(true)
			b.direct.Store(true)
			p := connect(t, ctx, a, b, fixed)
			x := startTransfer(ctx, p)
			defer x.finish()
			progress(t, ctx, x, 512<<10)
			if !fixed {
				check(t, waitUntil(ctx, func() bool { return a.maxSize.Load() > 1280 }))
				time.Sleep(100 * time.Millisecond)
			}
			a.mtu.Store(1200)
			b.mtu.Store(1200)
			start := x.received.Load()
			switched := time.Now()
			time.Sleep(600 * time.Millisecond)
			advanced := x.received.Load() - start
			t.Logf("hidden MTU drop ->1200: progress=%d elapsed=%s max_packet=%d oversize_drops=%d %s", advanced, time.Since(switched), a.maxSize.Load(), a.oversize.Load(), p.trace.snapshot())
			if fixed {
				if advanced < 1<<20 || a.oversize.Load() != 0 {
					t.Fatal("fixed-size path failed")
				}
			} else {
				if a.oversize.Load() == 0 {
					t.Fatal("did not exercise oversized packets")
				}
			}
			// Restore MTU to release the active writer; this tests recovery too.
			a.mtu.Store(frameMax)
			b.mtu.Store(frameMax)
			progress(t, ctx, x, 256<<10)
			x.finish()
			select {
			case err := <-x.done:
				check(t, err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func TestOutagePastIdle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := makePair(t)
	a.direct.Store(true)
	b.direct.Store(true)
	p := connect(t, ctx, a, b, true)
	x := startTransfer(ctx, p)
	defer x.finish()
	progress(t, ctx, x, 128<<10)
	a.blackhole.Store(true)
	b.blackhole.Store(true)
	start := time.Now()
	select {
	case <-p.client.Context().Done():
		t.Logf("inner connection expired after %s; request resume/new connection required", time.Since(start))
	case <-ctx.Done():
		t.Fatal("idle timeout did not close connection")
	}
}
