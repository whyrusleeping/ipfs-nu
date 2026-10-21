//go:build linux && natlab

package natlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/proto/autonat"
)

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func endpoint(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

type world struct {
	lab                               *Lab
	internet, router, public, private *Namespace
	lan                               *Segment
}

func topology(t *testing.T) *world {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	l, err := New(ctx)
	check(t, err)
	t.Cleanup(l.Close)
	internet, err := l.Namespace("internet")
	check(t, err)
	segment, err := internet.Segment("br0", "198.18.0.1/24")
	check(t, err)
	router, err := l.Namespace("router")
	check(t, err)
	check(t, segment.Attach(router, "wan", "198.18.0.20/24", ""))
	lan, err := router.Segment("lan", "10.0.0.1/24")
	check(t, err)
	public, err := l.Namespace("public")
	check(t, err)
	check(t, segment.Attach(public, "eth0", "198.18.0.10/24", ""))
	check(t, public.Address("eth0", "", "198.18.0.11/24"))
	private, err := l.Namespace("private")
	check(t, err)
	check(t, lan.Attach(private, "eth0", "10.0.0.2/24", "10.0.0.1"))
	return &world{l, internet, router, public, private, lan}
}
func start(t *testing.T, n *Namespace, c HostConfig) *Node {
	t.Helper()
	node, err := n.StartHost(c)
	check(t, err)
	return node
}
func emit(t *testing.T, result Outcome) {
	t.Helper()
	b, err := json.Marshal(result)
	check(t, err)
	t.Log(string(b))
	if path := os.Getenv("NU_NATLAB_RESULTS"); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		check(t, err)
		_, err = f.Write(append(b, '\n'))
		check(t, err)
		check(t, f.Close())
	}
}

// AutoNAT must dial a fresh public source socket: a NAT request connection must
// not accidentally make that candidate appear reachable. This is 0012's netns
// acceptance check, not a UDP sink approximation.
func TestAutoNATReachability(t *testing.T) {
	if !Isolate(t) {
		return
	}
	started := time.Now()
	w := topology(t)
	_, err := w.router.NAT(NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr("198.18.0.20")})
	check(t, err)
	observer := start(t, w.public, HostConfig{Listen: "198.18.0.10:4000", AutoNATServer: true})
	for _, tc := range []struct {
		name, listen string
		ns           *Namespace
		want         autonat.Reachability
	}{
		{"public", "198.18.0.11:4001", w.public, autonat.Reachable},
		{"nat", "10.0.0.2:4001", w.private, autonat.Unreachable},
	} {
		node := start(t, tc.ns, HostConfig{Listen: tc.listen})
		check(t, node.Dial(observer.Address))
		observed, err := observer.Remote(node.ID)
		check(t, err)
		ep := endpoint(observed)
		if tc.name == "nat" && ep.Addr().String() != "198.18.0.20" {
			t.Fatalf("request bypassed NAT: %s", ep)
		}
		candidate := fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", ep.Addr(), ep.Port())
		got, err := node.Check(observer.Address, candidate)
		check(t, err)
		if got != tc.want {
			t.Fatalf("%s: want %v, got %v", tc.name, tc.want, got)
		}
		result := "reachable"
		if got == autonat.Unreachable {
			result = "unreachable"
		}
		emit(t, Outcome{Scenario: "autonat-v2", From: tc.name, To: "observer", Outcome: result, Elapsed: time.Since(started)})
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("startup classification exceeded 5 seconds")
	}
}

func TestMappingAndFiltering(t *testing.T) {
	if !Isolate(t) {
		return
	}
	// Three representatives, not an exhaustive 3x3 matrix. The knobs are
	// independent; packets, not the name of the nft rules, prove each claim.
	for _, tc := range []struct {
		mapping Mapping
		filter  Filtering
	}{
		{EndpointIndependent, FilterEndpointIndependent},
		{AddressDependent, FilterAddressDependent},
		{AddressPortDependent, FilterAddressPortDependent},
	} {
		t.Run(string(tc.mapping), func(t *testing.T) {
			begin := time.Now()
			w := topology(t)
			destinations := []netip.AddrPort{endpoint("198.18.0.10:9000"), endpoint("198.18.0.10:9001"), endpoint("198.18.0.11:9000")}
			allocation := Preserve
			if tc.mapping == AddressPortDependent {
				allocation = Random
			}
			_, err := w.router.NAT(NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr("198.18.0.20"), Inside: []netip.AddrPort{endpoint("10.0.0.2:5000")}, Destinations: destinations, Mapping: tc.mapping, Filtering: tc.filter, Allocation: allocation})
			check(t, err)
			observer := start(t, w.public, HostConfig{Listen: "198.18.0.10:4000", Echo: []string{destinations[0].String(), destinations[1].String(), destinations[2].String()}})
			node := start(t, w.private, HostConfig{Listen: "10.0.0.2:4000", ProbeListen: "10.0.0.2:5000"})
			first, err := node.Probe(destinations[0].String())
			check(t, err)
			if allocation == Preserve && first != "198.18.0.20:5000" {
				t.Fatalf("port preservation: %s", first)
			}
			if allocation == Random && endpoint(first).Port() < 10000 {
				t.Fatalf("random allocation outside reserved range: %s", first)
			}
			// Filter probes precede contacting the alternate destinations: otherwise
			// they would prime exactly the pinholes this test is meant to measure.
			for i, source := range destinations {
				check(t, observer.SendFrom(source.String(), first))
				_, err := node.Receive()
				want := i == 0 || tc.filter == FilterEndpointIndependent || (i == 1 && tc.filter == FilterAddressDependent)
				if err != nil && !errors.Is(err, ErrTimeout) {
					t.Fatal(err)
				}
				if (err == nil) != want {
					t.Fatalf("filter %s from %s: accepted=%t want=%t err=%v", tc.filter, source, err == nil, want, err)
				}
			}
			second, err := node.Probe(destinations[1].String())
			check(t, err)
			third, err := node.Probe(destinations[2].String())
			check(t, err)
			switch tc.mapping {
			case EndpointIndependent:
				if first != second || first != third {
					t.Fatal(first, second, third)
				}
			case AddressDependent:
				if first != second || first == third {
					t.Fatal(first, second, third)
				}
			case AddressPortDependent:
				if first == second || first == third || second == third {
					t.Fatal(first, second, third)
				}
			}
			t.Logf("observer endpoints: %s %s %s", first, second, third)
			emit(t, Outcome{Scenario: string(tc.mapping) + "/" + string(tc.filter), From: "private", To: "public-observers", Outcome: "verified", Elapsed: time.Since(begin)})
		})
	}
}

func TestHairpinAndNetworkChanges(t *testing.T) {
	if !Isolate(t) {
		return
	}
	begin := time.Now()
	w := topology(t)
	cfg := NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr("198.18.0.20"), Mapping: EndpointIndependent, Filtering: FilterEndpointIndependent, Inside: []netip.AddrPort{endpoint("10.0.0.2:5000"), endpoint("10.0.0.3:5001")}, Destinations: []netip.AddrPort{endpoint("198.18.0.10:9000")}}
	start(t, w.public, HostConfig{Listen: "198.18.0.10:4000", Echo: []string{"198.18.0.10:9000"}})
	node := start(t, w.private, HostConfig{Listen: "10.0.0.2:4000", ProbeListen: "10.0.0.2:5000"})
	peerNS, err := w.lab.Namespace("shared-nat-peer")
	check(t, err)
	check(t, w.lan.Attach(peerNS, "eth0", "10.0.0.3/24", "10.0.0.1"))
	peer := start(t, peerNS, HostConfig{Listen: "10.0.0.3:4000", Echo: []string{"10.0.0.3:5001"}})
	for _, hairpin := range []bool{false, true} {
		cfg.Hairpin = hairpin
		nat, err := w.router.NAT(cfg)
		check(t, err)
		check(t, peer.SendFrom("10.0.0.3:5001", "198.18.0.10:9000"))
		_, err = node.Probe("198.18.0.20:5001")
		if err != nil && !errors.Is(err, ErrTimeout) {
			t.Fatal(err)
		}
		if (err == nil) != hairpin {
			t.Fatalf("hairpin=%t err=%v", hairpin, err)
		}
		check(t, nat.FlushMappings())
		// Existing sockets now establish new conntrack mappings to the new IP.
		check(t, w.router.Address("wan", "198.18.0.20/24", "198.18.0.21/24"))
		check(t, nat.Rebind(netip.MustParseAddr("198.18.0.21")))
		check(t, w.private.Link("eth0", false))
		check(t, w.private.Link("eth0", true))
		_, err = w.private.Run("ip", "route", "replace", "default", "via", "10.0.0.1")
		check(t, err)
		observed, err := node.Probe("198.18.0.10:9000")
		check(t, err)
		if observed != "198.18.0.21:5000" {
			t.Fatal("rebind:", observed)
		}
		check(t, w.router.Address("wan", "198.18.0.21/24", "198.18.0.20/24"))
		check(t, nat.FlushMappings())
		check(t, w.router.NFT("delete table ip nu_natlab"))
	}
	cfg.BlockUDP = true
	nat, err := w.router.NAT(cfg)
	check(t, err)
	_, err = node.Probe("198.18.0.10:9000")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("UDP block: want timeout, got %v", err)
	}
	check(t, nat.FlushMappings())
	emit(t, Outcome{Scenario: "hairpin-rebind-link-udp-block", From: "private", To: "shared-nat-peer/public", Outcome: "verified", Elapsed: time.Since(begin)})
}
