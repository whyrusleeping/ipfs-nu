package quicgocid

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
	hookms "github.com/whyrusleeping/nu/experiments/quicgo-cid/magicsock"
	"github.com/whyrusleeping/nu/nutest/slow"
	stockms "github.com/whyrusleeping/nu/p2p/magicsock"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// vsock abstracts the stock (qlog observer) and hook-based magicsock so both run
// the identical benchmark. conn keeps the concrete *Conn as quic-go's PacketConn.
type vsock struct {
	conn      net.PacketConn
	close     func() error
	addPeer   func(wire.PeerID) (netip.AddrPort, error)
	config    func(net.Addr, *quic.Config) *quic.Config
	attach    func(wire.PeerID, io.ReadWriteCloser) error
	setDirect func(wire.PeerID, []netip.AddrPort) error
	useDirect func(wire.PeerID, netip.AddrPort) error
	stats     func() (directSent, dropped uint64, cids int)
}

func newStock(u *net.UDPConn) (*vsock, error) {
	c, err := stockms.New(stockms.Config{UDP: u})
	if err != nil {
		return nil, err
	}
	return &vsock{conn: c, close: c.Close, addPeer: c.AddPeer, config: c.QUICConfigForPeer,
		attach:    func(id wire.PeerID, s io.ReadWriteCloser) error { _, err := c.AttachRelay(id, s); return err },
		setDirect: c.SetDirectPaths, useDirect: c.UseDirect,
		stats: func() (uint64, uint64, int) { s := c.Stats(); return s.DirectSent, s.Dropped, s.CIDs }}, nil
}

func newHook(u *net.UDPConn) (*vsock, error) {
	c, err := hookms.New(hookms.Config{UDP: u})
	if err != nil {
		return nil, err
	}
	return &vsock{conn: c, close: c.Close, addPeer: c.AddPeer, config: c.QUICConfigForPeer,
		attach:    func(id wire.PeerID, s io.ReadWriteCloser) error { _, err := c.AttachRelay(id, s); return err },
		setDirect: c.SetDirectPaths, useDirect: c.UseDirect,
		stats: func() (uint64, uint64, int) { s := c.Stats(); return s.DirectSent, s.Dropped, s.CIDs }}, nil
}

func benchTLS(tb testing.TB) (*tls.Config, wire.PeerID) {
	tb.Helper()
	key, err := wire.GenerateNodeKey()
	if err != nil {
		tb.Fatal(err)
	}
	cert, err := wire.NewTLSCertificate(key, time.Now())
	if err != nil {
		tb.Fatal(err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{wire.TLSALPN}, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAnyClientCert, InsecureSkipVerify: true, VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
		_, err := wire.VerifyTLSCertificate(raw, "", time.Now(), 16<<10)
		return err
	}}, key.Public().PeerID()
}

type pair struct {
	client, server *quic.Conn
	ma, mb         *vsock
	cleanup        []func()
}

func (p *pair) Close() {
	for i := len(p.cleanup) - 1; i >= 0; i-- {
		p.cleanup[i]()
	}
}

// setup mirrors experiments/nupath BenchmarkProductionPaths exactly: same TLS,
// 1200-byte fixed packets with PMTU discovery off (except RawPMTU), eight-byte
// local CIDs, 1 MiB stream / 4 MiB connection receive-window caps.
func setup(tb testing.TB, ctx context.Context, name string, extra func(*quic.Config)) *pair {
	tb.Helper()
	p := &pair{}
	fail := func(err error) {
		if err != nil {
			p.Close()
			tb.Fatal(err)
		}
	}
	ta, aid := benchTLS(tb)
	tbc, bid := benchTLS(tb)
	ua, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	fail(err)
	p.cleanup = append(p.cleanup, func() { ua.Close() })
	ub, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	fail(err)
	p.cleanup = append(p.cleanup, func() { ub.Close() })
	var ca, cb net.PacketConn = ua, ub
	remote := ub.LocalAddr()
	acfg := &quic.Config{InitialPacketSize: 1200, DisablePathMTUDiscovery: true, MaxStreamReceiveWindow: 1 << 20, MaxConnectionReceiveWindow: 4 << 20}
	switch name {
	case "RawPMTU":
		acfg.InitialPacketSize = 0
		acfg.DisablePathMTUDiscovery = false
	case "RawObserver":
		acfg.Tracer = func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace { return discardQUICTrace{} }
	case "RawCIDTracer":
		acfg.ConnectionIDTracer = func(context.Context, bool, quic.ConnectionID) *quic.ConnectionIDTracer {
			return &quic.ConnectionIDTracer{AddedConnectionID: func(quic.ConnectionID) {}, RemovedConnectionID: func(quic.ConnectionID) {}}
		}
	}
	if extra != nil {
		extra(acfg)
	}
	bcfg := acfg.Clone()
	if name == "VirtualDirectQlog" || name == "VirtualDirect" {
		mk := newHook
		if name == "VirtualDirectQlog" {
			mk = newStock
		}
		p.ma, err = mk(ua)
		fail(err)
		p.cleanup = append(p.cleanup, func() { p.ma.close() })
		p.mb, err = mk(ub)
		fail(err)
		p.cleanup = append(p.cleanup, func() { p.mb.close() })
		aaddr, err := p.ma.addPeer(bid)
		fail(err)
		baddr, err := p.mb.addPeer(aid)
		fail(err)
		remote = net.UDPAddrFromAddrPort(aaddr)
		ca, cb = p.ma.conn, p.mb.conn
		acfg = p.ma.config(remote, acfg)
		bcfg = p.mb.config(net.UDPAddrFromAddrPort(baddr), bcfg)
		ra, rb := net.Pipe()
		fail(p.ma.attach(bid, ra))
		fail(p.mb.attach(aid, rb))
	}
	at := &quic.Transport{Conn: ca, ConnectionIDLength: 8}
	bt := &quic.Transport{Conn: cb, ConnectionIDLength: 8}
	p.cleanup = append(p.cleanup, func() { at.Close() }, func() { bt.Close() })
	listener, err := bt.Listen(tbc, bcfg)
	fail(err)
	p.client, err = at.Dial(ctx, remote, ta, acfg)
	fail(err)
	p.cleanup = append(p.cleanup, func() { p.client.CloseWithError(0, "") })
	p.server, err = listener.Accept(ctx)
	fail(err)
	p.cleanup = append(p.cleanup, func() { p.server.CloseWithError(0, "") })
	if p.ma != nil {
		pa, pb := ub.LocalAddr().(*net.UDPAddr).AddrPort(), ua.LocalAddr().(*net.UDPAddr).AddrPort()
		fail(p.ma.setDirect(bid, []netip.AddrPort{pa}))
		fail(p.mb.setDirect(aid, []netip.AddrPort{pb}))
		fail(p.ma.useDirect(bid, pa))
		fail(p.mb.useDirect(aid, pb))
	}
	return p
}

// sink accepts uni streams on server and reports each stream's byte count check.
func sink(ctx context.Context, server *quic.Conn, size int64) <-chan error {
	done := make(chan error, 1)
	go func() {
		for {
			s, err := server.AcceptUniStream(ctx)
			if err != nil {
				return
			}
			n, copyErr := io.Copy(io.Discard, s)
			if copyErr == nil && n != size {
				copyErr = io.ErrUnexpectedEOF
			}
			done <- copyErr
		}
	}()
	return done
}

func send(ctx context.Context, client *quic.Conn, payload []byte) error {
	s, err := client.OpenUniStreamSync(ctx)
	if err != nil {
		return err
	}
	if _, err = s.Write(payload); err != nil {
		return err
	}
	return s.Close()
}

// BenchmarkCIDTracer reruns the 0015 method (docs/validation.md) against the
// patched quic-go. Opt-in: it is a throughput measurement, not a correctness gate.
//
//	Raw               unwrapped UDP, no observer
//	RawObserver       Raw + no-op qlog recorder (old CID-observer floor)
//	RawCIDTracer      Raw + no-op ConnectionIDTracer (new hook floor)
//	VirtualDirectQlog nu/p2p/magicsock (qlog observer), direct path
//	VirtualDirect     hook-based magicsock copy, direct path
//	RawPMTU           stock packet policy control
func BenchmarkCIDTracer(b *testing.B) {
	slow.Skip(b)
	for _, name := range []string{"Raw", "RawObserver", "RawCIDTracer", "VirtualDirectQlog", "VirtualDirect", "RawPMTU"} {
		b.Run(name, func(b *testing.B) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			p := setup(b, ctx, name, nil)
			defer p.Close()
			const size = 4 << 20
			payload := make([]byte, size)
			done := sink(ctx, p.server, size)
			b.SetBytes(size)
			b.ResetTimer()
			for range b.N {
				if err := send(ctx, p.client, payload); err != nil {
					b.Fatal(err)
				}
				if err := <-done; err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if p.client.ConnectionState().GSO && p.server.ConnectionState().GSO {
				b.ReportMetric(1, "GSO")
			}
			stats := p.client.ConnectionStats()
			b.ReportMetric(float64(stats.BytesSent)/float64(stats.PacketsSent), "tx-bytes/pkt")
			b.ReportMetric(float64(stats.PacketsLost+p.server.ConnectionStats().PacketsLost)/float64(b.N), "lost/op")
			if p.ma != nil {
				_, da, _ := p.ma.stats()
				_, db, _ := p.mb.stats()
				b.ReportMetric(float64(da+db)/float64(b.N), "adapter-drops/op")
			}
		})
	}
}

// TestVirtualDirectCIDRotation is the fast acceptance check for the hook-based
// observer: a transfer long enough for both peers to switch connection IDs
// (quic-go rotates every 5k-15k packets) keeps flowing on the direct path, the
// receiver registers replacement CIDs, and every route is released on close.
func TestVirtualDirectCIDRotation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var mu sync.Mutex
	var added, removed int
	p := setup(t, ctx, "VirtualDirect", func(c *quic.Config) {
		c.ConnectionIDTracer = func(context.Context, bool, quic.ConnectionID) *quic.ConnectionIDTracer {
			return &quic.ConnectionIDTracer{
				AddedConnectionID:   func(quic.ConnectionID) { mu.Lock(); added++; mu.Unlock() },
				RemovedConnectionID: func(quic.ConnectionID) { mu.Lock(); removed++; mu.Unlock() },
			}
		}
	})
	defer p.Close()
	if p.client.ConnectionState().Used0RTT || p.client.RemoteAddr().(*net.UDPAddr).AddrPort().Port() != 1 {
		t.Fatal("not dialed over a virtual address")
	}
	const size = 24 << 20 // ~21k 1200-byte packets
	done := sink(ctx, p.server, size)
	sentBefore, _, _ := p.ma.stats()
	if err := send(ctx, p.client, make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sentAfter, _, cids := p.ma.stats()
	// A rotated CID that magicsock failed to register would reach quic-go from
	// the physical address and migrate the server off the virtual address.
	if ra := p.server.RemoteAddr().(*net.UDPAddr).AddrPort(); ra.Port() != 1 || !ra.Addr().Is4() || ra.Addr().As4()[0]>>4 != 15 {
		t.Fatalf("server path left the virtual address: %v", ra)
	}
	if sentAfter-sentBefore < size/1200 {
		t.Fatalf("transfer did not use the direct path: %d direct datagrams", sentAfter-sentBefore)
	}
	mu.Lock()
	n := added
	mu.Unlock()
	// Handshake: client 1 CID, server 2 (its own + the client's original DCID).
	// Each side then issues active_connection_id_limit(4)-1 = 3 more. Anything
	// beyond those 9 is a replacement for a CID the peer retired.
	if n <= 9 {
		t.Fatalf("no CID rotation observed: %d added", n)
	}
	if cids == 0 || cids > 64 {
		t.Fatalf("unexpected routed CID count %d", cids)
	}
	p.client.CloseWithError(0, "")
	select {
	case <-p.server.Context().Done():
	case <-ctx.Done():
		t.Fatal("server did not observe close")
	}
	_, _, ca := p.ma.stats()
	_, _, cb := p.mb.stats()
	mu.Lock()
	a, r := added, removed
	mu.Unlock()
	if ca != 0 || cb != 0 || a != r {
		t.Fatalf("routes retained after close: %d/%d CIDs, %d added, %d removed", ca, cb, a, r)
	}
}

// discardQUICTrace forces stock qlog event construction without doing any work.
type discardQUICTrace struct{}

func (discardQUICTrace) SupportsSchemas(s string) bool      { return s == qlog.EventSchema }
func (d discardQUICTrace) AddProducer() qlogwriter.Recorder { return d }
func (discardQUICTrace) RecordEvent(qlogwriter.Event)       {}
func (discardQUICTrace) Close() error                       { return nil }
