//go:build linux && natlab

package disco

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/natlab"
	"github.com/whyrusleeping/nu/p2p/netcheck"
)

func TestHardObserverHelper(t *testing.T) {
	if os.Getenv("NU_HARD_OBSERVER") != "1" {
		return
	}
	set := netcheck.EndpointSet{Primary: netip.MustParseAddrPort("198.18.0.1:5000"), AltPort: netip.MustParseAddrPort("198.18.0.1:5001"), AltIP: netip.MustParseAddrPort("198.18.0.2:5000")}
	s, err := netcheck.NewServer(netcheck.ServerConfig{Sets: []netcheck.ServerSet{{Listen: set}}, PerIPBurst: 100, PerIPPerSecond: 100})
	check(t, err)
	defer s.Close()
	check(t, json.NewEncoder(os.Stdout).Encode("ready"))
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// Dynamic nft NAT: numgen runs only on the first packet of each conntrack
// tuple. Unlike natlab's preallocated fixtures, every fresh destination or
// source socket consumes a mapping at run time. APDM inbound is conntrack-only
// (APDF). EIM uses port-preserving SNAT and active-port DNAT, with an explicit
// learned endpoint filter when requested. One inside host avoids port conflicts.
func hardLabNAT(t *testing.T, router *natlab.Namespace, inside, external, allocation, filter string) {
	t.Helper()
	_, err := router.Run("sysctl", "-qw", "net.ipv4.ip_forward=1", "net.ipv4.conf.all.rp_filter=0", "net.ipv4.conf.default.rp_filter=0")
	check(t, err)
	if allocation == "eim" {
		guard := ""
		if filter == "apdf" {
			guard = "meta l4proto udp ct original proto-dst . ip saddr . udp sport @allowed "
		}
		check(t, router.NFT(fmt.Sprintf(`table ip hard {
set active { type inet_service; flags dynamic,timeout; timeout 60s; size 4096; }
set allowed { type inet_service . ipv4_addr . inet_service; flags dynamic,timeout; timeout 60s; size 8192; }
chain pre { type nat hook prerouting priority dstnat; iifname "wan" ip daddr %s udp dport @active dnat to %s; }
chain forward { type filter hook forward priority filter; policy drop;
iifname "lan" meta l4proto udp update @active { udp sport } update @allowed { udp sport . ip daddr . udp dport } accept;
ct state established,related accept;
ct status dnat %saccept; }
chain post { type nat hook postrouting priority srcnat; oifname "wan" meta l4proto udp snat to %s; }
}`, external, inside, guard, external)))
		return
	}
	// An inet_service-valued map ensures network byte order at the NAT ABI.
	// A bare integer numgen expression is byte-swapped by this kernel/nft pair.
	var entries []string
	for i := range 1024 {
		entries = append(entries, fmt.Sprintf("%d : %d", i, 10000+i))
	}
	maps := "map ports { typeof numgen inc mod 1024 : udp dport; elements = { " + strings.Join(entries, ",") + " }; }"
	generator := "numgen inc mod 1024 map @ports"
	if allocation == "random" {
		maps = ""
		generator = "1024-65535 fully-random"
	}
	check(t, router.NFT(fmt.Sprintf(`table ip hard {
%s
chain forward { type filter hook forward priority filter; policy drop; iifname "lan" accept; ct state established,related accept; }
chain post { type nat hook postrouting priority srcnat; oifname "wan" meta l4proto udp snat to %s : %s; }
}`, maps, external, generator)))
}

func TestHardNATLab(t *testing.T) {
	if !natlab.IsolateTimeout(t, 30*time.Minute) {
		return
	}
	trials := 3
	if text := os.Getenv("NU_HARD_TRIALS"); text != "" {
		n, err := strconv.Atoi(text)
		check(t, err)
		if n < 1 || n > 200 {
			t.Fatal("NU_HARD_TRIALS must be 1..200")
		}
		trials = n
	}
	for _, pair := range [][3]string{{"sequential", "eim", "eif"}, {"sequential", "eim", "apdf"}, {"random", "eim", "eif"}, {"random", "eim", "apdf"}, {"sequential", "sequential", "apdf"}, {"sequential", "random", "apdf"}, {"random", "random", "apdf"}, {"random", "eim", "ips"}} {
		if selected := os.Getenv("NU_HARD_CASE"); selected != "" && selected != strings.Join(pair[:], "-") {
			continue
		}
		t.Run(pair[0]+"-"+pair[1]+"-"+pair[2], func(t *testing.T) {
			success := 0
			for trial := 0; trial < trials; trial++ {
				t.Run(strconv.Itoa(trial), func(t *testing.T) {
					if hardLabCase(t, pair) {
						success++
					}
				})
			}
			t.Logf("MEASURE %v: %d/%d both-direct; failures retain relay (not a waived success target)", pair, success, trials)
		})
	}
}
func hardLabCase(t *testing.T, pair [3]string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	l, err := natlab.New(ctx)
	check(t, err)
	defer l.Close()
	internet, err := l.Namespace("internet")
	check(t, err)
	wan, err := internet.Segment("wan", "198.18.0.1/24")
	check(t, err)
	_, err = internet.Run("ip", "addr", "add", "198.18.0.2/24", "dev", "wan")
	check(t, err)
	in, out, err := internet.Start("env", "NU_HARD_OBSERVER=1", os.Args[0], "-test.run=^TestHardObserverHelper$")
	check(t, err)
	defer in.Close()
	var ready string
	check(t, json.NewDecoder(out).Decode(&ready))
	observers := []netcheck.Observer{{EndpointSet: netcheck.EndpointSet{Primary: netip.MustParseAddrPort("198.18.0.1:5000"), AltPort: netip.MustParseAddrPort("198.18.0.1:5001"), AltIP: netip.MustParseAddrPort("198.18.0.2:5000")}}}
	var clients [2]*natlab.Namespace
	var cfg [2]labConfig
	for i := range clients {
		router, e := l.Namespace("router")
		check(t, e)
		external := fmt.Sprintf("198.18.0.%d", 20+i)
		check(t, wan.Attach(router, "wan", external+"/24", ""))
		lan, e := router.Segment("lan", fmt.Sprintf("10.%d.0.1/24", i))
		check(t, e)
		clients[i], e = l.Namespace("peer")
		check(t, e)
		inside := fmt.Sprintf("10.%d.0.2", i)
		check(t, lan.Attach(clients[i], "eth0", inside+"/24", fmt.Sprintf("10.%d.0.1", i)))
		hardLabNAT(t, router, inside, external, pair[i], pair[2])
		cfg[i] = labConfig{Advertise: external + ":4000", Hard: true, Observers: observers}
		if pair[i] == "sequential" {
			cfg[i].Noise = 200 * time.Millisecond
		}
	}
	peers := startLabPair(t, clients, cfg)
	for i, p := range peers {
		r := p.call(t, labCommand{Op: "profile"}).Report
		t.Logf("peer%d measured: %s; last allocation=%v", i, r.Profile, r.IPv4.Samples)
		if pair[i] != "eim" && r.Profile.Mapping != netcheck.MappingAddressPortDependent {
			t.Fatalf("fixture did not measure APDM: %s", r.Profile)
		}
		if pair[i] == "sequential" && r.Profile.Allocation != netcheck.AllocSequential {
			t.Fatalf("fixture did not measure sequential allocation: %s", r.Profile)
		}
		if pair[i] == "random" && r.Profile.Allocation != netcheck.AllocRandom {
			t.Fatalf("fixture did not measure random allocation: %s", r.Profile)
		}
	}
	if pair[2] == "ips" {
		check(t, clients[0].NFT(`table inet ips {
 chain output { type filter hook output priority filter; meta l4proto udp @th,64,32 0x016e7564 drop; }
}`))
	}
	start := time.Now()
	peers[0].call(t, labCommand{Op: "open"})
	var a, b Status
	for time.Since(start) < 17*time.Second {
		a = peers[0].call(t, labCommand{Op: "status"}).Status
		b = peers[1].call(t, labCommand{Op: "status"}).Status
		if a.Selected.IsValid() && b.Selected.IsValid() {
			break
		}
		if a.HardBackoffs > 0 && b.HardBackoffs > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	success := a.Selected.IsValid() && b.Selected.IsValid()
	t.Logf("direct=%t elapsed=%v; peer0=%+v peer1=%+v", success, time.Since(start), a, b)
	for _, st := range []Status{a, b} {
		if st.HardSocketPeak > hardSocketLimit || st.HardSent > hardPacketLimit || st.HardAttempts > 1 {
			t.Fatal("hard bounds exceeded", st)
		}
	}
	if pair[0] != "random" || pair[1] != "random" {
		if a.HardAttempts != 1 || b.HardAttempts != 1 {
			t.Fatal("extension did not negotiate an attempt", a, b)
		}
	} else if a.HardSent != 0 || b.HardSent != 0 || a.HardAttempts != 0 || b.HardAttempts != 0 {
		t.Fatal("both-random must stay relayed")
	}
	if pair[2] == "ips" && (success || a.HardBackoffs != 1 || b.HardBackoffs != 1 || a.HardSockets != 0 || b.HardSockets != 0) {
		t.Fatal("IPS-like drop did not cancel/back off and release sockets", a, b)
	}
	if pair[0] == "sequential" && pair[1] == "eim" && !success {
		t.Fatal("predictable APDM/EIM did not recover")
	}
	// Whether direct or relayed, the same inner connection must still work.
	peers[0].call(t, labCommand{Op: "transfer", Bytes: 64 << 10})
	r := peers[0].call(t, labCommand{Op: "result"})
	if r.Progress != 64<<10 || !r.Alive {
		t.Fatal("inner transfer did not survive", r)
	}
	return success
}
