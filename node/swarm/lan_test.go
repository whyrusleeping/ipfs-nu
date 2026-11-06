package swarm

import (
	"bytes"
	"context"
	"net"
	"testing"

	"github.com/whyrusleeping/nu/nutest/timeouts"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// A fresh node gets a bare CID from a LAN holder it only heard over mDNS: no link
// hints, no connection, no DHT (0083).
func TestBareCIDFromMDNSPeer(t *testing.T) {
	iface := loopbackMulticast(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.Ceiling())
	defer cancel()
	start := func() *Swarm {
		k, err := wire.GenerateNodeKey()
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(t.TempDir(), store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		s, err := Start(ctx, Config{Key: k, Store: st, Listen: []string{"/ip4/127.0.0.1/udp/0/quic-v1"}, MDNS: true,
			MDNSInterfaces: []net.Interface{iface}, Logger: discard})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	holder, getter := start(), start()
	data := []byte("found on the LAN")
	c, err := holder.cfg.Store.AddBlob(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := getter.Fetch(ctx, c, nil, nil); err != nil {
		t.Fatalf("bare-CID fetch from an mDNS peer: %v", err)
	}
	if got, err := getter.cfg.Store.Get(c); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("fetched %q, %v", got, err)
	}
}
