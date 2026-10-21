package kadinterop

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	goCID "github.com/ipfs/go-cid"
	libp2p "github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	mh "github.com/multiformats/go-multihash"

	"github.com/whyrusleeping/nu/kad"
	nuhost "github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestAminoProviderInteropBothDirections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	gA, rA := newGoDHT(t, dht.ModeServer)
	gB, rB := newGoDHT(t, dht.ModeClient)
	goInfo := peer.AddrInfo{ID: gA.ID(), Addrs: quicOnly(gA.Addrs())}
	if len(goInfo.Addrs) == 0 {
		t.Fatal("go host has no QUIC address")
	}
	if err := gB.Connect(ctx, goInfo); err != nil {
		t.Fatal(err)
	}
	if err := rB.Bootstrap(ctx); err != nil {
		t.Logf("go client bootstrap: %v", err)
	}
	waitPeers(t, func() int { return rB.RoutingTable().Size() })

	nKey, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	nh, err := nuhost.New(nuhost.Config{Key: nKey, Listen: []string{"127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nh.Close() })
	nuAddr, err := wire.ParseMultiaddr(nh.Addrs()[0].String()+"/p2p/"+nh.ID().String(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	goPeer := goPeerID(t, gA.ID())
	goAddrs := wireAddrs(t, goInfo)
	ns, err := nuhost.NewSessions(nh, nuhost.SessionConfig{Peers: map[wire.PeerID][]wire.Multiaddr{goPeer: goAddrs}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	nuDHT, err := kad.New(ctx, kad.Config{Host: nh, Sessions: ns, PubliclyReachable: true, Bootstrap: []kad.Peer{{ID: goPeer, Addrs: byteAddrs(goAddrs)}}, AllowPrivateAddresses: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nuDHT.Close() })
	// Establish the reverse direction so go-libp2p's server DHT learns Nu.
	nuPeer, err := peer.Decode(nh.ID().String())
	if err != nil {
		t.Fatal(err)
	}
	if err = gA.Connect(ctx, peer.AddrInfo{ID: nuPeer, Addrs: goAddrsForNu(t, nuAddr)}); err != nil {
		t.Fatal(err)
	}
	if _, err = rA.RoutingTable().TryAddPeer(nuPeer, true, true); err != nil {
		t.Fatal(err)
	}

	nuKey := makeKey(t, 0x1e)
	if err = nuDHT.Provide(ctx, nuKey, []wire.Multiaddr{nuAddr}); err != nil {
		t.Fatalf("Nu provide to go-libp2p: %v", err)
	}
	found := false
	for info := range rB.FindProvidersAsync(ctx, goCIDFromBytes(t, nuKey), 8) {
		if info.ID == nuPeer {
			found = true
		}
	}
	if !found {
		t.Fatal("go-libp2p did not find Nu provider")
	}

	goKey := makeKey(t, mh.SHA2_256)
	if err = rA.Provide(ctx, goCIDFromBytes(t, goKey), true); err != nil {
		t.Fatalf("go provide to Nu: %v", err)
	}
	got, err := nuDHT.FindProviders(ctx, goKey)
	t.Logf("Nu provider lookup returned %d providers: %v (%v)", len(got), got, err)
	if err != nil {
		t.Fatalf("Nu find go provider: %v", err)
	}
	found = false
	for _, p := range got {
		if p.ID == goPeer {
			found = true
		}
	}
	if !found {
		t.Fatalf("Nu did not find Go provider: %v", got)
	}
}

func newGoDHT(t *testing.T, mode dht.ModeOpt) (host.Host, *dht.IpfsDHT) {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/udp/0/quic-v1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	r, err := dht.New(h, dht.Mode(mode))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return h, r
}
func waitPeers(t *testing.T, f func() int) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for f() == 0 && time.Now().Before(until) {
		time.Sleep(25 * time.Millisecond)
	}
	if f() == 0 {
		t.Fatal("DHT routing table stayed empty")
	}
}
func goPeerID(t *testing.T, id peer.ID) wire.PeerID {
	t.Helper()
	p, e := wire.ParsePeerID(id.String())
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func wireAddrs(t *testing.T, info peer.AddrInfo) []wire.Multiaddr {
	t.Helper()
	var out []wire.Multiaddr
	for _, a := range info.Addrs {
		m, e := wire.ParseMultiaddr(a.String()+"/p2p/"+info.ID.String(), 1024)
		if e == nil {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		t.Fatal("no QUIC address")
	}
	return out
}
func quicOnly(in []ma.Multiaddr) []ma.Multiaddr {
	var out []ma.Multiaddr
	for _, a := range in {
		if strings.Contains(a.String(), "/quic-v1") {
			out = append(out, a)
		}
	}
	return out
}
func goAddrsForNu(t *testing.T, a wire.Multiaddr) []ma.Multiaddr {
	t.Helper()
	m, e := ma.NewMultiaddrBytes(a.Bytes())
	if e != nil {
		t.Fatal(e)
	}
	parts := ma.Split(m)
	plain := m.Decapsulate(parts[len(parts)-1].Multiaddr())
	out, e := ma.NewMultiaddr(plain.String())
	if e != nil {
		t.Fatal(e)
	}
	return []ma.Multiaddr{out}
}
func byteAddrs(in []wire.Multiaddr) [][]byte {
	out := make([][]byte, len(in))
	for i, a := range in {
		out[i] = a.Bytes()
	}
	return out
}
func makeKey(t *testing.T, code uint64) []byte {
	t.Helper()
	digest := make([]byte, 32)
	if _, e := rand.Read(digest); e != nil {
		t.Fatal(e)
	}
	b, e := mh.Encode(digest, code)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func goCIDFromBytes(t *testing.T, b []byte) goCID.Cid {
	t.Helper()
	return goCID.NewCidV1(goCID.Raw, mh.Multihash(b))
}
