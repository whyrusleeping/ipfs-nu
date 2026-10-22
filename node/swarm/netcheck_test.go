package swarm

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/netcheck"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/relay"
)

// 0018 seam: a public-mode daemon profiles against its configured relay's
// netcheck observer once connected, and serves the profile to peers.
func TestSwarmNATProfile(t *testing.T) {
	// No observers: the default adapter has no measurement, so status must
	// continue to show the explicitly labelled observation estimate.
	unmeasured := testSwarm(t, swarmKey(t))
	if n := unmeasured.ConnectivityStatus().NAT; n == nil || n.Source != "observations" {
		t.Fatalf("unmeasured fallback: %+v", n)
	}
	unmeasured.Close()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).AddrPort()
	pc.Close()
	k := swarmKey(t)
	r, err := relay.New(relay.Config{Host: host.Config{Key: swarmKey(t), Listen: []string{"127.0.0.1:0"}}, Server: circuit.ServerConfig{},
		Netcheck: &netcheck.ServerConfig{Sets: []netcheck.ServerSet{{Listen: netcheck.EndpointSet{Primary: netip.AddrPortFrom(port.Addr(), port.Port())}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := testSwarm(t, k, relayAddr(t, r))
	deadline := time.Now().Add(10 * time.Second)
	for {
		rep, ok, err := s.NATProfile()
		if ok {
			if rep.Profile.UDP != netcheck.UDPOK || rep.Profile.Mapping != netcheck.MappingNone {
				t.Fatalf("loopback profile: %s", rep.Profile)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no NAT profile: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	n := s.ConnectivityStatus().NAT
	if n == nil || n.Source != "netcheck" || n.Mapping != "none" || n.Observers != 1 || !strings.Contains(n.Detail, "udp=ok") || !strings.Contains(n.Detail, "allocation=") {
		t.Fatalf("status did not use measured NAT profile: %+v", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, ok, err := netcheck.FetchProfile(ctx, r.Host, s.ID())
	if err != nil || !ok || p.UDP != netcheck.UDPOK {
		t.Fatalf("exchanged profile: %s %t %v", p, ok, err)
	}
}
