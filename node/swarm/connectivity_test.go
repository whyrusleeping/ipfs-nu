package swarm

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/p2p/homerelay"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/relay"
)

func swarmKey(t *testing.T) *wire.NodeKey {
	t.Helper()
	k, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}
func relayFor(t *testing.T, ids ...wire.PeerID) *relay.Server {
	return relayWithDelay(t, 0, ids...)
}

// relayWithDelay simulates distance on loopback through ping reply latency.
func relayWithDelay(t *testing.T, delay time.Duration, ids ...wire.PeerID) *relay.Server {
	t.Helper()
	r, err := relay.New(relay.Config{Host: host.Config{Key: swarmKey(t), Listen: []string{"127.0.0.1:0"}}, Server: circuit.ServerConfig{Admitted: ids}, Core: proto.Config{PingReplyDelay: delay}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}
func relayAddr(t *testing.T, r *relay.Server) wire.Multiaddr {
	t.Helper()
	a, err := wire.ParseMultiaddr(r.Host.Addrs()[0].String()+"/p2p/"+r.Host.ID().String(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func testSwarm(t *testing.T, k *wire.NodeKey, relays ...wire.Multiaddr) *Swarm {
	t.Helper()
	s, err := Start(context.Background(), Config{Key: k, Relays: relays, Logger: discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func circuitHints(t *testing.T, ctx context.Context, s *Swarm, n int) []wire.Multiaddr {
	t.Helper()
	state, err := s.connectivity.homes.Wait(ctx, func(s homerelay.State) bool { return len(s.Addresses) >= n })
	if err != nil {
		t.Fatal(err)
	}
	return state.Addresses
}
func TestSwarmCircuitAssembly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	ka, kb := swarmKey(t), swarmKey(t)
	r := relayFor(t, ka.Public().PeerID(), kb.Public().PeerID())
	a, b := testSwarm(t, ka, relayAddr(t, r)), testSwarm(t, kb, relayAddr(t, r))
	hints := circuitHints(t, ctx, b, 1)
	if err := b.Host().Handle("/test/echo/1", 1, func(s *host.Stream) { io.Copy(s, s) }); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := a.Connect(ctx, b.ID(), hints); err != nil {
		t.Fatal(err)
	}
	conn, ok := a.Host().Connection(b.ID())
	if !ok || !a.Host().IsLocalOnlyAddress(conn.RemoteAddr()) {
		t.Fatal("not an authenticated inner connection")
	}
	stream, err := a.Sessions().OpenStream(ctx, b.ID(), "/test/echo/1")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	stream.SetDeadline(time.Now().Add(time.Second))
	if _, err = stream.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err = io.ReadFull(stream, buf); err != nil || string(buf) != "hello" {
		t.Fatal(string(buf), err)
	}
	firstEcho := time.Since(started)
	rtt, err := a.Ping(ctx, b.ID())
	if err != nil {
		t.Fatal(err)
	}
	peer, err := a.connectivity.peer(b.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.lock(ctx); err != nil {
		t.Fatal(err)
	}
	setup := peer.setupTime
	peer.unlock()
	// Allow scheduler/test-instrumentation noise, not a route stagger or a
	// foreground disco negotiation. The five-byte echo fits in one datagram.
	if setup <= 0 || firstEcho > setup+rtt+50*time.Millisecond {
		t.Fatalf("first byte delayed beyond setup + relay RTT: first=%v setup=%v rtt=%v", firstEcho, setup, rtt)
	}
	t.Logf("cold relay first echo=%v; measured circuit/inner handshake=%v; relay RTT=%v (50ms scheduler allowance)", firstEcho, setup, rtt)
	status := a.ConnectivityStatus()
	for _, path := range status.Paths {
		if path.PeerID == b.ID().Base36() && (path.Path != "relay" || path.Direct != "") {
			t.Fatal("status published a virtual key as a physical path", path)
		}
		// Doctor material: the session history records the circuit setup and Why
		// explains the relayed path (loopback candidates are private, hence undisclosed).
		if path.PeerID == b.ID().Base36() && (len(path.History) == 0 || !strings.Contains(path.History[0].Event, "relay circuit via "+r.Host.ID().Base36()) || path.Why == "") {
			t.Fatalf("session history/why missing: %+v", path)
		}
	}
	if status.UDP == nil || status.UDP.Outbound != "ok" || status.NAT == nil || status.PortMapping == nil || status.PortMapping.Enabled || status.IPv6 == nil {
		t.Fatalf("doctor diagnostics incomplete: %+v %+v %+v %+v", status.UDP, status.NAT, status.PortMapping, status.IPv6)
	}
	var reserved int
	for _, rs := range status.Relays {
		if rs.Reserved && rs.Role == "home" && !rs.HeldSince.IsZero() {
			reserved++
		}
	}
	if reserved != 1 {
		t.Fatalf("home reservation state not reported: %+v", status.Relays)
	}
	var m node.Metrics
	a.Metrics(ctx, &m)
	var page strings.Builder
	if _, err := m.WriteTo(&page); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# TYPE nu_net_bytes_total counter", `nu_net_bytes_total{direction="sent",path="relay"} `, `nu_net_paths{path="relay"} 1`, `nu_net_relay_reserved{relay="` + r.Host.ID().Base36() + `",role="home"} 1`, "nu_net_session_setup_seconds_count 1", `nu_net_probes_total{outcome="sent"}`} {
		if !strings.Contains(page.String(), want) {
			t.Fatalf("metrics page lacks %q:\n%s", want, page.String())
		}
	}
	if strings.Contains(page.String(), `nu_net_bytes_total{direction="sent",path="relay"} 0`) {
		t.Fatal("relayed bytes not counted")
	}
	for _, addr := range a.Addrs() {
		if a.Host().IsLocalOnlyMultiaddr(addr) {
			t.Fatal("virtual address published", addr)
		}
	}
}
