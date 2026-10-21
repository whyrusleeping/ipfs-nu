package swarm

import (
	"log/slog"
	"net"
	"testing"

	"github.com/whyrusleeping/nu/p2p/wire"
)

// Bound unspecified addresses advertise one address per interface IP of the family, or
// the family's loopback when there is none; concrete addresses are kept as bound.
func TestAdvertise(t *testing.T) {
	parse := func(s ...string) []wire.Multiaddr {
		var out []wire.Multiaddr
		for _, x := range s {
			a, err := wire.ParseMultiaddr(x, 1024)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, a)
		}
		return out
	}
	ips := func() []net.IP { return []net.IP{net.ParseIP("192.168.1.7"), net.ParseIP("10.0.0.2")} }
	got := Advertise(parse("/ip4/0.0.0.0/udp/4171/quic-v1", "/ip6/::/udp/4171/quic-v1", "/ip4/127.0.0.1/udp/5/quic-v1"), ips)
	want := []string{"/ip4/192.168.1.7/udp/4171/quic-v1", "/ip4/10.0.0.2/udp/4171/quic-v1", "/ip6/::1/udp/4171/quic-v1", "/ip4/127.0.0.1/udp/5/quic-v1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("addr %d: %s, want %s", i, got[i], want[i])
		}
	}
	if l, err := listenAddrs([]string{"/ip4/0.0.0.0/udp/4171/quic-v1", "/ip6/::/udp/4171/quic-v1", "/dns4/x/udp/1/quic-v1", "/ip4/1.2.3.4/udp/9/quic-v1"}, discard); err != nil ||
		len(l) != 2 || l[0] != "0.0.0.0:4171" || l[1] != "[::]:4171" {
		t.Fatalf("listenAddrs: %v %v", l, err)
	}
}

var discard = slog.New(slog.DiscardHandler)
