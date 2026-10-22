package swarm

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/homerelay"
	"github.com/whyrusleeping/nu/p2p/host"
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
	t.Helper()
	r, err := relay.New(relay.Config{Host: host.Config{Key: swarmKey(t), Listen: []string{"127.0.0.1:0"}}, Server: circuit.ServerConfig{Admitted: ids}})
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
	if err := a.Connect(ctx, b.ID(), circuitHints(t, ctx, b, 1)); err != nil {
		t.Fatal(err)
	}
	conn, ok := a.Host().Connection(b.ID())
	if !ok || !a.Host().IsLocalOnlyAddress(conn.RemoteAddr()) {
		t.Fatal("not an authenticated inner connection")
	}
	if _, err := a.Ping(ctx, b.ID()); err != nil {
		t.Fatal(err)
	}
	if err := b.Host().Handle("/test/echo/1", 1, func(s *host.Stream) { io.Copy(s, s) }); err != nil {
		t.Fatal(err)
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
	for _, addr := range a.Addrs() {
		if a.Host().IsLocalOnlyMultiaddr(addr) {
			t.Fatal("virtual address published", addr)
		}
	}
}
