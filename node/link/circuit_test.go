package link

import (
	"testing"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestCircuitHintBoundToProvider(t *testing.T) {
	relay, peer, other := testPeer(t), testPeer(t), testPeer(t)
	text := "/ip4/198.18.0.2/udp/4000/quic-v1/p2p/" + relay.String() + "/p2p-circuit/p2p/" + peer.String()
	addr, err := wire.ParseMultiaddr(text, MaxAddrLen)
	if err != nil {
		t.Fatal(err)
	}
	l := Link{CID: cid.MustParse(rawCID), Providers: []Provider{{Peer: peer, Addrs: []wire.Multiaddr{addr}}}}
	parsed, err := Parse(l.String())
	if err != nil || parsed.Providers[0].Addrs[0].String() != text {
		t.Fatal("circuit share link failed to round trip", err)
	}
	l.Providers[0].Peer = other
	if _, err := Parse(l.String()); err == nil {
		t.Fatal("circuit destination mismatch accepted")
	}
	l.Providers[0].Peer = peer
	stacked, err := wire.ParseMultiaddr(text+"/p2p-circuit/p2p/"+peer.String(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	l.Providers[0].Addrs = []wire.Multiaddr{stacked}
	if _, err := Parse(l.String()); err == nil {
		t.Fatal("unsupported stacked circuit accepted")
	}
}
