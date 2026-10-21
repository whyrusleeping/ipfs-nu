package qpath_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/magicsock"
	nuproto "github.com/whyrusleeping/nu/p2p/proto"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/proto/qpath"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func udp(t *testing.T, addr string) *net.UDPConn {
	t.Helper()
	a, err := net.ResolveUDPAddr("udp", addr)
	check(t, err)
	c, err := net.ListenUDP("udp", a)
	check(t, err)
	t.Cleanup(func() { c.Close() })
	return c
}
func newHost(t *testing.T, socket net.PacketConn) (*host.Host, *wire.NodeKey) {
	t.Helper()
	key, err := wire.GenerateNodeKey()
	check(t, err)
	cfg := host.Config{Key: key}
	if socket != nil {
		cfg.PacketConns = []net.PacketConn{socket}
	}
	h, err := host.New(cfg)
	check(t, err)
	t.Cleanup(func() { h.Close() })
	return h, key
}
func newMagic(t *testing.T, u *net.UDPConn) (*host.Host, *magicsock.Conn) {
	t.Helper()
	c, err := magicsock.New(magicsock.Config{UDP: u})
	check(t, err)
	t.Cleanup(func() { c.Close() })
	h, _ := newHost(t, c)
	return h, c
}
func address(t *testing.T, h *host.Host) wire.Multiaddr {
	t.Helper()
	a, err := wire.ParseMultiaddr(h.Addrs()[0].String()+"/p2p/"+h.ID().String(), 1024)
	check(t, err)
	return a
}

type setup struct {
	a, b, r        *host.Host
	ma, mb         *magicsock.Conn
	ac, bc         *circuit.Client
	relayA, relayB *magicsock.Relay
	virtualA       wire.Multiaddr
}

func makeSetup(t *testing.T, ctx context.Context, ua, ub, ur *net.UDPConn) *setup {
	t.Helper()
	a, ma := newMagic(t, ua)
	b, mb := newMagic(t, ub)
	r, key := newHost(t, ur)
	server, err := circuit.NewServer(r, key, circuit.ServerConfig{Admitted: []wire.PeerID{a.ID(), b.ID()}})
	check(t, err)
	t.Cleanup(func() { server.Close() })
	ac, err := circuit.NewClient(a, circuit.ClientConfig{})
	check(t, err)
	t.Cleanup(func() { ac.Close() })
	bc, err := circuit.NewClient(b, circuit.ClientConfig{})
	check(t, err)
	t.Cleanup(func() { bc.Close() })
	_, err = a.Dial(ctx, address(t, r))
	check(t, err)
	_, err = b.Dial(ctx, address(t, r))
	check(t, err)
	_, err = bc.Reserve(ctx, r.ID())
	check(t, err)
	x := &setup{a: a, b: b, r: r, ma: ma, mb: mb, ac: ac, bc: bc}
	x.attach(t, ctx)
	return x
}
func (x *setup) attach(t *testing.T, ctx context.Context) {
	t.Helper()
	type result struct {
		r   *magicsock.Relay
		err error
	}
	incoming := make(chan result, 1)
	go func() {
		s, err := x.bc.Accept(ctx)
		if err != nil {
			incoming <- result{err: err}
			return
		}
		_, r, err := qpath.Accept(ctx, x.mb, s)
		incoming <- result{r, err}
	}()
	addr, r, err := qpath.Dial(ctx, x.ma, x.ac, x.r.ID(), x.b.ID())
	check(t, err)
	other := <-incoming
	check(t, other.err)
	x.relayA = r
	x.relayB = other.r
	x.virtualA = addr
}
func roundtrip(t *testing.T, s *host.Stream, n int) {
	t.Helper()
	block := bytes.Repeat([]byte("inner QUIC is end-to-end\n"), 512)
	for range n {
		_, err := s.Write(block)
		check(t, err)
		out := make([]byte, len(block))
		_, err = io.ReadFull(s, out)
		check(t, err)
		if !bytes.Equal(block, out) {
			t.Fatal("payload changed")
		}
	}
}
func (x *setup) stream(t *testing.T, ctx context.Context) *host.Stream {
	t.Helper()
	check(t, x.b.Handle("/nu/qpath-test/1", 2, func(s *host.Stream) { defer s.Close(); _, _ = io.Copy(s, s) }))
	cache, err := lookup.NewCache(ctx, lookup.CacheConfig{})
	check(t, err)
	sessions, err := host.NewSessions(x.a, host.SessionConfig{Cache: cache, Peers: map[wire.PeerID][]wire.Multiaddr{x.b.ID(): {x.virtualA}}})
	check(t, err)
	t.Cleanup(func() { sessions.Close() })
	session, err := sessions.Connect(ctx, x.b.ID())
	check(t, err)
	if len(cache.Addresses(x.b.ID())) != 0 {
		t.Fatal("virtual address persisted into hint cache")
	}
	if err := sessions.AddHints(ctx, []lookup.Hint{{Peer: x.b.ID(), Addresses: []wire.Multiaddr{x.virtualA}}}); err == nil {
		t.Fatal("virtual hint accepted from discovery")
	}
	s, err := session.OpenStream(ctx, "/nu/qpath-test/1")
	check(t, err)
	t.Cleanup(func() { s.Close() })
	deadline, _ := ctx.Deadline()
	check(t, s.SetDeadline(deadline))
	return s
}
func (x *setup) selectDirect(t *testing.T, aa, ba netip.AddrPort) {
	t.Helper()
	check(t, x.ma.SetDirectPaths(x.b.ID(), []netip.AddrPort{ba}))
	check(t, x.mb.SetDirectPaths(x.a.ID(), []netip.AddrPort{aa}))
	check(t, x.ma.UseDirect(x.b.ID(), ba))
	check(t, x.mb.UseDirect(x.a.ID(), aa))
}

func TestRelayDirectSharedSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	x := makeSetup(t, ctx, udp(t, "127.0.0.1:0"), udp(t, "127.0.0.1:0"), udp(t, "127.0.0.1:0"))
	s := x.stream(t, ctx)
	roundtrip(t, s, 4)
	before, ok := x.a.Connection(x.b.ID())
	if !ok || before.PeerID() != x.b.ID() {
		t.Fatal("missing authenticated inner peer")
	}
	if !magicsock.IsVirtual(before.RemoteAddr().(*net.UDPAddr).AddrPort()) {
		t.Fatal("inner not virtual")
	}
	serviceA, err := nuproto.New(x.a, nuproto.Config{Timeout: 100 * time.Millisecond})
	check(t, err)
	t.Cleanup(func() { serviceA.Close() })
	serviceB, err := nuproto.New(x.b, nuproto.Config{Timeout: 100 * time.Millisecond})
	check(t, err)
	t.Cleanup(func() { serviceB.Close() })
	info, err := serviceA.Identify(ctx, x.b.ID())
	check(t, err)
	if info.Message.ObservedAddr != nil {
		t.Fatal("identify leaked virtual observed address")
	}
	// Outer relay QUIC and inner QUIC occupy the SAME endpoint/socket/dispatcher.
	outer, ok := x.a.Connection(x.r.ID())
	if !ok || magicsock.IsVirtual(outer.RemoteAddr().(*net.UDPAddr).AddrPort()) {
		t.Fatal("outer address rewritten")
	}
	x.selectDirect(t, x.ma.LocalAddr().(*net.UDPAddr).AddrPort(), x.mb.LocalAddr().(*net.UDPAddr).AddrPort())
	roundtrip(t, s, 8)
	serverInner, ok := x.b.Connection(x.a.ID())
	if !ok || !magicsock.IsVirtual(serverInner.RemoteAddr().(*net.UDPAddr).AddrPort()) {
		t.Fatal("server lost virtual routing on direct switch")
	}
	keyA, err := before.ExportKeyingMaterial("nu-qpath-test-only", nil, 32)
	check(t, err)
	keyB, err := serverInner.ExportKeyingMaterial("nu-qpath-test-only", nil, 32)
	check(t, err)
	if !bytes.Equal(keyA, keyB) {
		t.Fatal("inner TLS exporter mismatch")
	}
	check(t, x.ma.WriteDisco([]byte("not authenticated; framing seam only"), x.mb.LocalAddr().(*net.UDPAddr).AddrPort()))
	buf := make([]byte, 1024)
	_, from, err := x.mb.ReadDisco(ctx, buf)
	check(t, err)
	if from != x.ma.LocalAddr().(*net.UDPAddr).AddrPort() {
		t.Fatal(from)
	}
	// The CID observer must include issued/rotated IDs, not only handshake SCIDs.
	if x.ma.Stats().CIDs < 2 || x.mb.Stats().CIDs < 2 {
		t.Fatal("NEW_CONNECTION_ID not registered", x.ma.Stats(), x.mb.Stats())
	}
	check(t, x.ma.UseRelay(x.b.ID()))
	check(t, x.mb.UseRelay(x.a.ID()))
	roundtrip(t, s, 4)
	check(t, x.relayA.Close())
	check(t, x.relayB.Close())
	x.attach(t, ctx)
	roundtrip(t, s, 4)
	after, ok := x.a.Connection(x.b.ID())
	if !ok || before != after {
		t.Fatal("inner connection replaced")
	}
	if x.ma.Stats().DirectSent == 0 || x.ma.Stats().Relayed == 0 {
		t.Fatal("path unused")
	}
	check(t, s.CloseWrite())
	tail, err := io.ReadAll(s)
	check(t, err)
	if len(tail) != 0 {
		t.Fatal("trailing data")
	}
	t.Logf("one stream survived relay/direct/relay/reattach; shared socket stats A=%+v B=%+v", x.ma.Stats(), x.mb.Stats())
}
