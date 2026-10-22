package swarm

import (
	"net"
	"net/netip"
	"testing"

	"github.com/whyrusleeping/nu/p2p/host"
)

func TestPublicationRequiresIndependentPublicProofs(t *testing.T) {
	observerA, observerB := netip.MustParseAddr("198.18.0.2"), netip.MustParseAddr("198.18.0.3")
	proofs := map[netip.AddrPort]map[netip.Addr]bool{
		netip.MustParseAddrPort("198.18.1.1:4171"): {observerA: true}, // multiple IDs at one IP still one proof
		netip.MustParseAddrPort("198.18.1.2:4171"): {observerA: true, observerB: true},
		netip.MustParseAddrPort("10.0.0.2:4171"):   {observerA: true, observerB: true},
		netip.MustParseAddrPort("240.0.0.1:1"):     {observerA: true, observerB: true},
	}
	confirmed := confirmedAddresses(proofs)
	if len(confirmed) != 1 || confirmed[0].String() != "/ip4/198.18.1.2/udp/4171/quic-v1" {
		t.Fatal("invalid proof aggregation", confirmed)
	}
	h, _, err := sharedHost(host.Config{Key: swarmKey(t), Listen: []string{"0.0.0.0:0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	// A stopped-clock publisher fixture isolates snapshot policy from background
	// AutoNAT/monitor work, whose nonce verification has separate protocol tests.
	s := &Swarm{host: h, cfg: Config{InterfaceIPs: func() []net.IP { return []net.IP{net.ParseIP("240.0.0.1"), net.ParseIP("10.0.0.2")} }}}
	c := &connectivity{s: s}
	n := &networkControl{c: c, confirmed: confirmed, reach: "public"}
	c.network = n
	s.connectivity = c
	if shared := s.Addrs(); len(shared) != 2 || shared[0].String() != confirmed[0].String() {
		t.Fatal("verified mapped address missing from user links", shared)
	}
	published := s.publicationAddrs()
	if len(published) != 1 || published[0].String() != confirmed[0].String() {
		t.Fatal("LAN/loopback literal disclosed in a public signed snapshot", published)
	}
	n.mu.Lock()
	n.confirmed = nil
	n.mu.Unlock()
	if published := s.publicationAddrs(); len(published) != 0 {
		t.Fatal("stale proof or LAN address remained published", published)
	}
	if shared := s.Addrs(); len(shared) != 1 {
		t.Fatal("explicit user LAN link was lost, or a virtual key was published", shared)
	}
}
