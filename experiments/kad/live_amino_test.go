package kadinterop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	nuproto "github.com/whyrusleeping/nu/p2p/proto"
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
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	bootstrap := pinnedLiveBootstrappers(ctx, t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newLiveNuNode(t, ctx, bootstrap)
			b := newLiveNuNode(t, ctx, bootstrap)
			key := tc.mh.Bytes()
			addrs, bootstrapID, remoteInfo, err := observedProviderAddresses(ctx, a, bootstrap)
			if err != nil {
				t.Fatalf("obtain observed public provider address: %v", err)
			}
			if !slices.Contains(remoteInfo.Message.Protocols, kad.Protocol) {
				t.Fatalf("bootstrap peer %s did not identify Amino: %v", bootstrapID, remoteInfo.Message.Protocols)
			}
			start := time.Now()
			if err = a.dht.ProvideAndRepublish(ctx, key, addrs); err != nil {
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

// Use the interop agent's PeerId pins, but resolve the pinned DNS names to
// literal QUIC-v1 endpoints at runtime. No recursive Amino referral expansion.
func pinnedLiveBootstrappers(ctx context.Context, t *testing.T) []kad.Peer {
	t.Helper()
	data, err := os.ReadFile("../../interop/amino.json")
	if err != nil {
		t.Fatal(err)
	}
	var pins []struct {
		Host string `json:"host"`
		Port int    `json:"port"`
		Peer string `json:"peer"`
	}
	if err = json.Unmarshal(data, &pins); err != nil {
		t.Fatal(err)
	}
	out := make([]kad.Peer, 0, len(pins))
	for _, pin := range pins {
		id, err := wire.ParsePeerID(pin.Peer)
		if err != nil {
			t.Fatal(err)
		}
		resolveCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		ips, err := net.DefaultResolver.LookupIP(resolveCtx, "ip4", pin.Host)
		cancel()
		if err != nil {
			t.Errorf("resolve pinned bootstrap %s: %v", pin.Host, err)
			continue
		}
		if len(ips) == 0 {
			t.Errorf("pinned bootstrap %s has no IPv4", pin.Host)
			continue
		}
		addr, err := wire.ParseMultiaddr(fmt.Sprintf("/ip4/%s/udp/%d/quic-v1/p2p/%s", ips[0].To4(), pin.Port, id), 1024)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, kad.Peer{ID: id, Addrs: [][]byte{addr.Bytes()}})
	}
	if len(out) == 0 {
		t.Fatal("no usable pinned QUIC bootstrap addresses")
	}
	return out
}

type liveNode struct {
	h        *host.Host
	sessions *host.Sessions
	dht      *kad.Service
	core     *nuproto.Service
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
	peerMap := make(map[wire.PeerID][]wire.Multiaddr)
	for _, p := range bootstrap {
		for _, raw := range p.Addrs {
			a, e := wire.MultiaddrFromBytes(raw, 1024)
			if e == nil {
				peerMap[p.ID] = append(peerMap[p.ID], a)
			}
		}
	}
	sessions, err := host.NewSessions(h, host.SessionConfig{Peers: peerMap, ConnectTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessions.Close() })
	core, err := nuproto.New(h, nuproto.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	s, err := kad.New(ctx, kad.Config{Host: h, Sessions: sessions, Bootstrap: bootstrap, RequestTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, p := range h.Protocols() {
		if p == kad.Protocol {
			t.Fatalf("client mode advertises server protocol %s", kad.Protocol)
		}
	}
	return &liveNode{h: h, sessions: sessions, dht: s, core: core}
}
func observedProviderAddresses(ctx context.Context, n *liveNode, bootstrap []kad.Peer) ([]wire.Multiaddr, wire.PeerID, nuproto.PeerInfo, error) {
	var errs []error
	for _, p := range bootstrap {
		if _, err := n.sessions.Connect(ctx, p.ID); err != nil {
			errs = append(errs, fmt.Errorf("connect bootstrap %s: %w", p.ID, err))
			continue
		}
		info, err := n.core.Identify(ctx, p.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("identify bootstrap %s: %w", p.ID, err))
			continue
		}
		if info.Message.ObservedAddr == nil {
			errs = append(errs, fmt.Errorf("bootstrap %s omitted observed address", p.ID))
			continue
		}
		addr, err := lookup.NormalizeAddress(n.h.ID(), *info.Message.ObservedAddr)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !kad.IsQUICAddress(addr, n.h.ID()) {
			errs = append(errs, fmt.Errorf("observed address is not QUIC-v1: %s", addr))
			continue
		}
		return []wire.Multiaddr{addr}, p.ID, info, nil
	}
	return nil, "", nuproto.PeerInfo{}, errors.Join(errs...)
}
