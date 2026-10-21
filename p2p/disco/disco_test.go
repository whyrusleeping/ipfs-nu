package disco

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/magicsock"
	"github.com/whyrusleeping/nu/p2p/netmon"
	"github.com/whyrusleeping/nu/p2p/portmap"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func socket(t *testing.T) *net.UDPConn {
	t.Helper()
	u, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	check(t, err)
	t.Cleanup(func() { u.Close() })
	return u
}
func key(t *testing.T) *wire.NodeKey {
	t.Helper()
	k, err := wire.GenerateNodeKey()
	check(t, err)
	return k
}
func candidate(a netip.AddrPort) Candidate { return Candidate{LAN, a, time.Now().Add(time.Minute)} }
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if fn() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("condition timed out")
		case <-tick.C:
		}
	}
}

type endpoint struct {
	host   *host.Host
	socket *magicsock.Conn
	disco  *Service
	udp    netip.AddrPort
}

func newEndpoint(t *testing.T, k *wire.NodeKey, peer wire.PeerID, provide func(wire.PeerID) []Candidate) *endpoint {
	t.Helper()
	u := socket(t)
	m, err := magicsock.New(magicsock.Config{UDP: u})
	check(t, err)
	_, err = m.AddPeer(peer)
	check(t, err)
	h, err := host.New(host.Config{Key: k, PacketConns: []net.PacketConn{m}})
	check(t, err)
	t.Cleanup(func() { h.Close(); m.Close() })
	d, err := New(context.Background(), Config{Host: h, Socket: m, Candidates: provide, Authorize: func(p wire.PeerID, c Candidate) bool { return p == peer && c.Addr.Addr().IsLoopback() }, Heartbeat: 100 * time.Millisecond, ProbeTimeout: 100 * time.Millisecond})
	check(t, err)
	t.Cleanup(func() { d.Close() })
	return &endpoint{h, m, d, u.LocalAddr().(*net.UDPAddr).AddrPort()}
}
func innerPair(t *testing.T, provideA, provideB func(wire.PeerID) []Candidate) (*endpoint, *endpoint, *host.Conn) {
	t.Helper()
	ka, kb := key(t), key(t)
	a := newEndpoint(t, ka, kb.Public().PeerID(), provideA)
	b := newEndpoint(t, kb, ka.Public().PeerID(), provideB)
	ra, rb := net.Pipe()
	_, err := a.socket.AttachRelay(b.host.ID(), ra)
	check(t, err)
	_, err = b.socket.AttachRelay(a.host.ID(), rb)
	check(t, err)
	addr, _ := a.socket.PeerAddr(b.host.ID())
	ma, err := wire.ParseMultiaddr(fmt.Sprintf("/ip4/%s/udp/%d/quic-v1/p2p/%s", addr.Addr(), addr.Port(), b.host.ID()), 1024)
	check(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := a.host.Dial(ctx, ma)
	check(t, err)
	return a, b, c
}
func TestInnerUpgradeFallbackRecovery(t *testing.T) {
	var ca, cb atomic.Value
	ca.Store([]Candidate(nil))
	cb.Store([]Candidate(nil))
	a, b, c := innerPair(t, func(wire.PeerID) []Candidate { return ca.Load().([]Candidate) }, func(wire.PeerID) []Candidate { return cb.Load().([]Candidate) })
	ca.Store([]Candidate{candidate(a.udp)})
	cb.Store([]Candidate{candidate(b.udp)})
	check(t, b.host.Handle("/disco-test/1", 2, func(s *host.Stream) { _, _ = io.Copy(s, s) }))
	check(t, a.disco.Open(context.Background(), c))
	eventually(t, func() bool {
		sa, _ := a.disco.Status(b.host.ID())
		sb, _ := b.disco.Status(a.host.ID())
		return sa.Selected == b.udp && sb.Selected == a.udp
	})
	status, _ := a.disco.Status(b.host.ID())
	t.Logf("relay -> direct in %v", status.TimeToDirect)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := a.host.OpenStream(ctx, b.host.ID(), "/disco-test/1")
	check(t, err)
	defer stream.Close()
	check(t, stream.SetDeadline(time.Now().Add(3*time.Second)))
	// The same application stream and inner connection survive invalidation. Each
	// chunk is verified, not merely counted, including a chunk while relay-only.
	transfer := func(seed byte) {
		t.Helper()
		want := bytes.Repeat([]byte{seed}, 32<<10)
		done := make(chan error, 1)
		go func() { _, err := stream.Write(want); done <- err }()
		got := make([]byte, len(want))
		_, err := io.ReadFull(stream, got)
		check(t, err)
		check(t, <-done)
		if !bytes.Equal(want, got) {
			t.Fatal("transfer corruption")
		}
	}
	transfer(1)
	// Withdrawal must revoke path authorization, not only stop future pings.
	cb.Store([]Candidate(nil))
	b.disco.Update()
	eventually(t, func() bool { st, _ := a.disco.Status(b.host.ID()); return !st.Selected.IsValid() && st.Candidates == 0 })
	transfer(2)
	ca.Store([]Candidate{candidate(a.udp)})
	cb.Store([]Candidate{candidate(b.udp)})
	pipe, err := netmon.NewPipeline(time.Second)
	check(t, err)
	_, err = pipe.Register(netmon.RestartProbing, a.disco.RestartProbing)
	check(t, err)
	events := make(chan netmon.Event, 1)
	events <- netmon.Event{}
	close(events)
	check(t, pipe.Run(ctx, events))
	b.disco.Update()
	eventually(t, func() bool {
		sa, _ := a.disco.Status(b.host.ID())
		sb, _ := b.disco.Status(a.host.ID())
		return sa.Selected == b.udp && sb.Selected == a.udp
	})
	transfer(3)
	if c.Context().Err() != nil || stream.Conn() != c {
		t.Fatal("inner connection was replaced")
	}
	// Closing disco revokes only its paths, not the shared QUIC connection.
	check(t, a.disco.Close())
	eventually(t, func() bool { _, ok := b.disco.Status(a.host.ID()); return !ok })
	transfer(4)
	// A fresh stream on the SAME inner TLS connection has fresh replay keys.
	// Recreate only the local service, then reopen and retain the application stream.
	d, err := New(context.Background(), Config{Host: a.host, Socket: a.socket, Candidates: func(wire.PeerID) []Candidate { return ca.Load().([]Candidate) }, Authorize: func(_ wire.PeerID, c Candidate) bool { return c.Addr == b.udp }})
	check(t, err)
	defer d.Close()
	check(t, d.Open(ctx, c))
	eventually(t, func() bool { st, _ := d.Status(b.host.ID()); return st.Selected == b.udp })
	transfer(5)
}

func securityFixture(t *testing.T) (*Service, *session, *net.UDPConn) {
	t.Helper()
	m, err := magicsock.New(magicsock.Config{UDP: socket(t)})
	check(t, err)
	t.Cleanup(func() { m.Close() })
	id := key(t).Public().PeerID()
	_, err = m.AddPeer(id)
	check(t, err)
	dest := socket(t)
	a := dest.LocalAddr().(*net.UDPAddr).AddrPort()
	d := &Service{cfg: Config{Socket: m, Authorize: func(_ wire.PeerID, c Candidate) bool { return c.Addr == a }, ProbeTimeout: 100 * time.Millisecond}}
	s := &session{peer: id, remote: map[netip.AddrPort]Candidate{a: candidate(a)}, pending: make(map[[16]byte]challenge), proofs: make(map[netip.AddrPort]proof), tx: bytes.Repeat([]byte{1}, 32), rx: bytes.Repeat([]byte{2}, 32)}
	return d, s, dest
}
func TestProbeSecurityAndLimits(t *testing.T) {
	d, s, dest := securityFixture(t)
	from := dest.LocalAddr().(*net.UDPAddr).AddrPort()
	now := time.Now()
	ping := probe{kind: 1, seq: 1, session: s.id, challenge: [16]byte{7}}
	valid := encodeProbe(ping, s.rx)
	forged := bytes.Clone(valid)
	forged[65] ^= 1
	d.handleProbe(s, forged, from, now)
	if s.stats.Sent != 0 {
		t.Fatal("forged probe replied")
	}
	other := netip.AddrPortFrom(from.Addr(), from.Port()+1)
	d.handleProbe(s, valid, other, now)
	if s.stats.Sent != 0 {
		t.Fatal("unauthorized source replied")
	}
	d.handleProbe(s, valid, from, now)
	if s.stats.Sent != 1 {
		t.Fatal("valid ping not answered")
	}
	if len(s.proofs) != 0 {
		t.Fatal("ping installed path without round trip")
	}
	d.handleProbe(s, valid, from, now)
	if s.stats.Sent != 1 {
		t.Fatal("replay replied")
	}
	buf := make([]byte, 1500)
	check(t, dest.SetReadDeadline(time.Now().Add(time.Second)))
	n, _, err := dest.ReadFromUDP(buf)
	check(t, err)
	payload, err := magicsock.DecodeDisco(buf[:n])
	check(t, err)
	p, err := decodeProbe(payload, s.tx)
	check(t, err)
	if n != 103 || p.kind != 2 || p.observed != from || p.challenge != ping.challenge {
		t.Fatal("pong contract", n, p)
	}
	// Unknown challenges and right challenges arriving from a different authorized
	// endpoint cannot install a path. Neither observation is destination permission.
	pong := probe{kind: 2, seq: 2, session: s.id, challenge: [16]byte{9}, observed: from}
	d.handleProbe(s, encodeProbe(pong, s.rx), from, now)
	if len(s.proofs) != 0 {
		t.Fatal("unsolicited pong installed path")
	}
	s.pending[pong.challenge] = challenge{other, now}
	pong.seq++
	d.handleProbe(s, encodeProbe(pong, s.rx), from, now)
	if len(s.proofs) != 0 {
		t.Fatal("destination mismatch installed path")
	}
	s.pending[pong.challenge] = challenge{from, now.Add(-time.Second)}
	pong.seq++
	d.handleProbe(s, encodeProbe(pong, s.rx), from, now)
	if len(s.proofs) != 0 {
		t.Fatal("expired challenge installed path")
	}
	s.pending[pong.challenge] = challenge{from, now}
	pong.seq++
	d.handleProbe(s, encodeProbe(pong, s.rx), from, now)
	if len(s.proofs) != 1 || s.selected != from {
		t.Fatal("round trip not selected")
	}
	// A missed heartbeat revokes and falls back; old initial probes cannot kill a
	// path refreshed by a newer successful round trip.
	s.pending[[16]byte{10}] = challenge{from, now.Add(-time.Second)}
	d.prune(s, now)
	if s.selected != from {
		t.Fatal("stale pending ping killed newer proof")
	}
	s.pending[[16]byte{11}] = challenge{from, now}
	d.prune(s, now.Add(time.Second))
	if s.selected.IsValid() || len(s.proofs) != 0 {
		t.Fatal("heartbeat failure did not revoke path")
	}
	d.cfg.Authorize = func(wire.PeerID, Candidate) bool { return false }
	before := s.stats.Sent
	d.ping(s, from, now)
	if s.stats.Sent != before {
		t.Fatal("unauthorized destination probed")
	}
	d.cfg.Authorize = func(wire.PeerID, Candidate) bool { return true }
	for i := uint64(10); i < 200; i++ {
		ping.seq = i
		d.handleProbe(s, encodeProbe(ping, s.rx), from, now)
	}
	if s.stats.Sent != 32 {
		t.Fatalf("peer burst: sent %d, want 32", s.stats.Sent)
	}
	c := s.remote[from]
	c.Expires = now
	s.remote[from] = c
	before = s.stats.Sent
	ping.seq = 201
	d.handleProbe(s, encodeProbe(ping, s.rx), from, now)
	if s.stats.Sent != before {
		t.Fatal("expired source replied")
	}
	// Socket-wide cap is independent of peer buckets.
	var global bucket
	accepted := 0
	for range 200 {
		if global.take(now, 100, 128) {
			accepted++
		}
	}
	if accepted != 128 || !global.take(now.Add(10*time.Millisecond), 100, 128) {
		t.Fatal("global rate limit")
	}
	var w replayWindow
	for _, n := range []uint64{100, 102, 101, 40} {
		if !w.accept(n) {
			t.Fatal("valid reorder rejected", n)
		}
	}
	for _, n := range []uint64{0, 100, 101, 38} {
		if w.accept(n) {
			t.Fatal("replay accepted", n)
		}
	}
}
func TestContinuousSchedule(t *testing.T) {
	now := time.Unix(100, 0)
	s := &session{next: now}
	for wave := 0; wave < 5; wave++ {
		start := s.next
		for round, offset := range early {
			if s.next.Sub(start) != offset {
				t.Fatalf("wave %d round %d: offset %v", wave, round, s.next.Sub(start))
			}
			now = s.next
			advanceSchedule(s, now)
		}
		backoff := retries[min(wave, len(retries)-1)]
		if s.next.Sub(now) != backoff {
			t.Fatal("relayed retry stopped or wrong backoff")
		}
	}
	// A real invalidation restarts immediately, even at maximum backoff.
	(&Service{}).restartSchedule(s, now)
	if s.next != now || s.round != 0 || s.retry != 0 {
		t.Fatal("invalidation didn't restart")
	}
}

func TestPathHysteresis(t *testing.T) {
	d, s, dest := securityFixture(t)
	a := dest.LocalAddr().(*net.UDPAddr).AddrPort()
	b := netip.AddrPortFrom(a.Addr(), a.Port()+1)
	d.cfg.Authorize = func(wire.PeerID, Candidate) bool { return true }
	s.remote[b] = candidate(b)
	now := time.Now()
	s.proofs[a] = proof{now, 50 * time.Millisecond}
	s.selected = a
	s.selectedAt = now
	var nonce [16]byte
	pong := func(at time.Time, rtt time.Duration) {
		nonce[0]++
		s.pending[nonce] = challenge{b, at.Add(-rtt)}
		d.handleProbe(s, encodeProbe(probe{kind: 2, session: s.id, seq: uint64(nonce[0]), challenge: nonce, observed: a}, s.rx), b, at)
	}
	pong(now.Add(time.Second), time.Millisecond)
	if s.selected != a {
		t.Fatal("switched without dwell")
	}
	// A large improvement after the dwell switches; sub-5ms jitter does not.
	pong(now.Add(6*time.Second), time.Millisecond)
	if s.selected != b {
		t.Fatal("better path not selected")
	}
	s.proofs[a] = proof{now.Add(6 * time.Second), 5 * time.Millisecond}
	s.selected = a
	s.selectedAt = now
	pong(now.Add(7*time.Second), time.Millisecond)
	if s.selected != a {
		t.Fatal("jitter flapped selected path")
	}
}

func TestMappingAndControl(t *testing.T) {
	now := time.Now()
	a := netip.MustParseAddrPort("192.0.2.1:1234")
	remote := netip.MustParseAddrPort("192.0.2.2:5678")
	m := portmap.Mapping{External: a, Remote: remote, Expires: now.Add(time.Minute)}
	if _, ok := FromMapping(m, a, now); ok {
		t.Fatal("PCP PEER restriction ignored")
	}
	c, ok := FromMapping(m, remote, now)
	if !ok {
		t.Fatal("mapping lost")
	}
	if _, ok := FromMapping(m, remote, m.Expires); ok {
		t.Fatal("expired mapping")
	}
	b, err := encodeControl(1, []Candidate{c}, now)
	check(t, err)
	gen, list, err := decodeControl(b, now)
	check(t, err)
	if gen != 1 || len(list) != 1 || list[0].Addr != a {
		t.Fatal("control roundtrip")
	}
	for _, addr := range []string{"0.0.0.0:3", "255.255.255.255:3", "224.0.0.1:3", "240.0.0.1:3", "169.254.1.1:3", "[fe80::1]:3", "[::ffff:127.0.0.1]:3", "[100::1]:3"} {
		if validEndpoint(netip.MustParseAddrPort(addr)) {
			t.Fatal("bad destination", addr)
		}
	}
	b[11] = 5
	if _, _, err := decodeControl(b, now); err == nil {
		t.Fatal("unnegotiated predicted kind")
	}
	b[11] = byte(Mapped)
	binary.BigEndian.PutUint16(b[12:14], 601)
	if _, _, err := decodeControl(b, now); err == nil {
		t.Fatal("unbounded lease")
	}
	if _, err := readControl(bytes.NewReader([]byte{255, 255})); err == nil {
		t.Fatal("oversized frame allocated")
	}
}

type fakeExporter struct{}

func (fakeExporter) ExportKeyingMaterial(label string, ctx []byte, n int) ([]byte, error) {
	var out []byte
	for i := byte(0); len(out) < n; i++ {
		sum := sha256.Sum256(append(append([]byte(label), ctx...), i))
		out = append(out, sum[:]...)
	}
	return out[:n], nil
}
func TestExporterSessionBinding(t *testing.T) {
	a, b := key(t).Public().PeerID(), key(t).Public().PeerID()
	na, nb := [16]byte{1}, [16]byte{2}
	id, tx, rx, err := keys(fakeExporter{}, a, b, na, nb)
	check(t, err)
	id2, tx2, rx2, err := keys(fakeExporter{}, b, a, nb, na)
	check(t, err)
	if id != id2 || bytes.Equal(tx, rx) || !bytes.Equal(tx, rx2) || !bytes.Equal(rx, tx2) {
		t.Fatal("directional keys don't agree")
	}
	na[0]++
	id3, _, _, err := keys(fakeExporter{}, a, b, na, nb)
	check(t, err)
	if id == id3 {
		t.Fatal("control stream reused exporter keys")
	}
}
func FuzzDecoders(f *testing.F) {
	now := time.Unix(100, 0)
	key := bytes.Repeat([]byte{42}, 32)
	control, _ := encodeControl(1, []Candidate{{LAN, netip.MustParseAddrPort("127.0.0.1:1"), now.Add(time.Minute)}}, now)
	for _, b := range [][]byte{control, encodeProbe(probe{kind: 1, seq: 1}, key), encodeProbe(probe{kind: 2, seq: 2, observed: netip.MustParseAddrPort("[2001:db8::1]:44")}, key), {1, 0}, {255, 255}, {}} {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if gen, list, err := decodeControl(b, now); err == nil {
			out, err := encodeControl(gen, list, now)
			check(t, err)
			if !bytes.Equal(b, out) {
				t.Fatal("control not canonical")
			}
		}
		if p, err := decodeProbe(b, key); err == nil {
			if !bytes.Equal(b, encodeProbe(p, key)) {
				t.Fatal("probe not canonical")
			}
		}
		_, _ = readControl(bytes.NewReader(b))
	})
}
