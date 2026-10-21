package kad

import (
	"context"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
)

type testNode struct {
	h        *host.Host
	sessions *host.Sessions
	dht      *Service
	addr     wire.Multiaddr
	server   bool
}

func makeNode(t *testing.T, server bool) *testNode {
	t.Helper()
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	h, err := host.New(host.Config{Key: key, Listen: []string{"127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	sm, err := host.NewSessions(h, host.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	a, err := wire.ParseMultiaddr(h.Addrs()[0].String()+"/p2p/"+h.ID().String(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	return &testNode{h: h, sessions: sm, addr: a, server: server}
}

func mesh(t *testing.T, nodes ...*testNode) {
	t.Helper()
	for i, n := range nodes {
		var peers []Peer
		for j, p := range nodes {
			if i != j && p.server {
				peers = append(peers, Peer{ID: p.h.ID(), Addrs: [][]byte{p.addr.Bytes()}})
			}
		}
		s, err := New(context.Background(), Config{Host: n.h, Sessions: n.sessions, PubliclyReachable: n.server, Bootstrap: peers, AllowPrivateAddresses: true})
		if err != nil {
			t.Fatal(err)
		}
		n.dht = s
		t.Cleanup(func() { _ = s.Close() })
	}
	for _, n := range nodes {
		for _, p := range nodes {
			if n != p {
				if err := n.sessions.SetPeer(p.h.ID(), []wire.Multiaddr{p.addr}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestSmallDHTProvideFindClientAndServerModes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, b, c := makeNode(t, true), makeNode(t, true), makeNode(t, false)
	mesh(t, a, b, c)
	key := []byte{0x1e, 32}
	for i := 0; i < 32; i++ {
		key = append(key, byte(i+1))
	}
	if err := a.dht.Provide(ctx, key, []wire.Multiaddr{a.addr}); err != nil {
		t.Fatal(err)
	}
	got, err := c.dht.FindProviders(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("provider not found")
	}
	found := false
	for _, p := range got {
		if p.ID == a.h.ID() {
			found = true
		}
	}
	if !found {
		t.Fatalf("provider absent: %#v", got)
	}
	if len(c.h.Protocols()) != 0 {
		t.Fatal("client-only node registered server protocol")
	}
	if len(a.h.Protocols()) != 1 {
		t.Fatal("reachable server failed to register KAD")
	}
	if _, err = c.dht.FindNode(ctx, b.h.ID()); err != nil {
		t.Fatal(err)
	}
	lookedUp := false
	if err = c.dht.Lookup(ctx, b.h.ID(), func(a lookup.Address) bool { lookedUp = a.Peer == b.h.ID(); return true }); err != nil {
		t.Fatal(err)
	}
	if !lookedUp {
		t.Fatal("FIND_PEER source did not return target peer")
	}
}

func TestDHTPrivateDisabled(t *testing.T) {
	key, _ := wire.GenerateNodeKey()
	h, err := host.New(host.Config{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	sessions, err := host.NewSessions(h, host.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	if _, err = New(context.Background(), Config{Host: h, Sessions: sessions, Private: true}); err == nil {
		t.Fatal("private DHT constructed")
	}
}
