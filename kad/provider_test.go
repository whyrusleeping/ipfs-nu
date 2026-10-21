package kad

import (
	"context"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestProviderTTLAndGlobalRecordBound(t *testing.T) {
	local, provider, other := makeNode(t, true), makeNode(t, false), makeNode(t, false)
	s, err := New(context.Background(), Config{Host: local.h, Sessions: local.sessions, PubliclyReachable: true, MaxProviders: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	key1 := []byte{0x12, 32}
	for i := 0; i < 32; i++ {
		key1 = append(key1, byte(i))
	}
	key2 := append([]byte(nil), key1...)
	key2[2]++
	p1 := Peer{ID: provider.h.ID(), Addrs: [][]byte{provider.addr.Bytes()}}
	p2 := Peer{ID: other.h.ID(), Addrs: [][]byte{other.addr.Bytes()}}
	if !s.storeProvider(key1, p1) {
		t.Fatal("first provider rejected")
	}
	if s.storeProvider(key2, p2) {
		t.Fatal("provider record cap not enforced")
	}
	entry := s.providers[string(key1)]
	entry.expires[p1.ID] = time.Now().Add(-time.Second)
	s.providers[string(key1)] = entry
	if got := s.getProviders(key1); len(got) != 0 {
		t.Fatal("expired record returned")
	}
	if s.providerRecords != 0 {
		t.Fatalf("expired provider records remain: %d", s.providerRecords)
	}
	if !s.storeProvider(key2, p2) {
		t.Fatal("expired record did not release capacity")
	}
}

func TestSetPubliclyReachableChangesHandler(t *testing.T) {
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
	s, err := New(context.Background(), Config{Host: h, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(h.Protocols()) != 0 {
		t.Fatal("client mode handler registered")
	}
	if err = s.SetPubliclyReachable(true); err != nil {
		t.Fatal(err)
	}
	if len(h.Protocols()) != 1 {
		t.Fatal("server mode not enabled")
	}
	if err = s.SetPubliclyReachable(false); err != nil {
		t.Fatal(err)
	}
	if len(h.Protocols()) != 0 {
		t.Fatal("server mode not disabled")
	}
}
