package kadinterop

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestLiveNuAminoProviderKeys(t *testing.T) {
	if os.Getenv("NU_AMINO") != "1" {
		t.Skip("set NU_AMINO=1 to opt in to external Amino DHT operations")
	}
	recipe := []byte("nu.follow\x00did:plc:ewvi7nxzyoun6zhxrhs64oiz\x00amino-spike-dataset")
	cases := []struct {
		name string
		mh   cid.Multihash
	}{
		{"sha256-control", cid.Sum(cid.SHA256, []byte("nu-amino-live-control"))},
		{"random-blake3", cid.Sum(cid.BLAKE3, []byte("nu-amino-live-random-payload"))},
		{"follow-swarm-blake3", cid.Sum(cid.BLAKE3, recipe)},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	dns, err := lookup.NewDNS(lookup.DNSConfig{})
	if err != nil {
		t.Fatal(err)
	}
	bootstrapper := newLiveNuNode(t, ctx, nil)
	if err = bootstrapper.dht.BootstrapDNS(ctx, dns); err != nil {
		t.Fatalf("default dnsaddr bootstrap resolution: %v", err)
	}
	bootstrapPeers := bootstrapper.dht.Table().Peers()
	if len(bootstrapPeers) == 0 {
		t.Fatal("dnsaddr bootstrap returned no QUIC-v1 peers")
	}
	bootstrap := make([]kad.Peer, len(bootstrapPeers))
	for i, p := range bootstrapPeers {
		bootstrap[i] = kad.Peer{ID: p.ID, Addrs: p.Addrs}
	}
	_ = bootstrapper.dht.Close()
	_ = bootstrapper.sessions.Close()
	_ = bootstrapper.h.Close()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newLiveNuNode(t, ctx, bootstrap)
			b := newLiveNuNode(t, ctx, bootstrap)
			key := tc.mh.Bytes()
			addrs := withPeer(t, a.h)
			start := time.Now()
			if err := a.dht.ProvideAndRepublish(ctx, key, addrs); err != nil {
				t.Fatalf("Nu provide: %v", err)
			}
			provideTime := time.Since(start)
			start = time.Now()
			providers, err := b.dht.FindProviders(ctx, key)
			findTime := time.Since(start)
			if err != nil {
				t.Fatalf("independent Nu GET_PROVIDERS: %v", err)
			}
			found := false
			for _, p := range providers {
				if p.ID == a.h.ID() {
					found = true
				}
			}
			t.Logf("key=%s providers=%d provider_peer=%s query_peer=%s provide=%s find=%s found=%t", tc.mh, len(providers), a.h.ID(), b.h.ID(), provideTime, findTime, found)
			if !found {
				t.Fatalf("independent Nu client did not find advertised key %x", key)
			}
		})
	}
}

type liveNode struct {
	h        *host.Host
	sessions *host.Sessions
	dht      *kad.Service
}

func newLiveNuNode(t *testing.T, ctx context.Context, bootstrap []kad.Peer) *liveNode {
	t.Helper()
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	h, err := host.New(host.Config{Key: key, Listen: []string{"0.0.0.0:0"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	sessions, err := host.NewSessions(h, host.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessions.Close() })
	s, err := kad.New(ctx, kad.Config{Host: h, Sessions: sessions, Bootstrap: bootstrap, RequestTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &liveNode{h, sessions, s}
}
func withPeer(t *testing.T, n *host.Host) []wire.Multiaddr {
	t.Helper()
	var out []wire.Multiaddr
	for _, a := range n.Addrs() {
		m, err := wire.ParseMultiaddr(a.String()+"/p2p/"+n.ID().String(), 1024)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}
