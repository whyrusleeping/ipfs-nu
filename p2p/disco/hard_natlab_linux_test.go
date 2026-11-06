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

// hardLabSides are the per-side NAT kinds of the pair matrix. Every APDM side
// uses APDF filtering; "public" has no NAT at all. Labels are the table's.
var hardLabSides = map[string]string{"public": "public", "eim-eif": "EIM/EIF", "eim-apdf": "EIM/APDF", "sequential": "APDM-seq/APDF", "random": "APDM-random/APDF"}

func eimLike(kind string) bool { return kind == "public" || strings.HasPrefix(kind, "eim") }

// hardLabWant mirrors the per-pair acceptance (0025/0097): direct whenever one
// side is EIM/public, except EIM/APDF <-> random APDM, which is a birthday
// search held to >= 98 % by the table tool over a campaign; APDM <-> APDM is
// reported (relay fallback), not gated.
func hardLabWant(a, b string) string {
	switch {
	case !eimLike(a) && !eimLike(b):
		return "relay"
	case a == "random" && b == "eim-apdf" || b == "random" && a == "eim-apdf":
		return "98%"
	}
	return "direct"
}

func TestHardNATLab(t *testing.T) {
	if !natlab.IsolateTimeout(t, 55*time.Minute) {
		return
	}
	trials := hardLabTrials(t, "NU_HARD_TRIALS", 3)
	campaign := hardLabTrials(t, "NU_HARD_RANDOM_TRIALS", trials)
	kinds := []string{"public", "eim-eif", "eim-apdf", "sequential", "random"}
	type row struct{ a, b, scenario string }
	var rows []row
	for i := range kinds {
		for j := i; j < len(kinds); j++ {
			rows = append(rows, row{kinds[j], kinds[i], "pair"})
		}
	}
	rows = append(rows, row{"random", "eim-apdf", "ips-drop"})
	for _, r := range rows {
		name := r.a + "+" + r.b
		if r.scenario != "pair" {
			name = r.scenario + ":" + name
		}
		if selected := os.Getenv("NU_HARD_CASE"); selected != "" && selected != name {
			continue
		}
		t.Run(name, func(t *testing.T) {
			want, n := hardLabWant(r.a, r.b), trials
			if r.scenario != "pair" {
				want = "relay"
			} else if want == "98%" || (r.a == "random" && eimLike(r.b)) {
				n = campaign
			}
			success := 0
			for trial := 0; trial < n; trial++ {
				t.Run(strconv.Itoa(trial), func(t *testing.T) {
					start := time.Now()
					ok, ttd := hardLabCase(t, r.a, r.b, r.scenario == "ips-drop")
					outcome := "relayed"
					if ok {
						success++
						outcome = "direct"
					}
					check(t, natlab.Record(natlab.Outcome{Scenario: r.scenario, From: hardLabSides[r.a], To: hardLabSides[r.b], Outcome: outcome, Elapsed: time.Since(start), TimeToDirect: ttd}))
				})
			}
			t.Logf("MEASURE %s %s <-> %s: %d/%d both-direct (want %s)", r.scenario, hardLabSides[r.a], hardLabSides[r.b], success, n, want)
			if want == "direct" && success != n {
				t.Errorf("%d/%d direct; this pair must always go direct", success, n)
			}
		})
	}
}
func hardLabTrials(t *testing.T, env string, def int) int {
	text := os.Getenv(env)
	if text == "" {
		return def
	}
	n, err := strconv.Atoi(text)
	check(t, err)
	if n < 1 || n > 200 {
		t.Fatalf("%s must be 1..200", env)
	}
	return n
}

// hardLabCase builds a fresh topology and identities, then reports whether both
// sides selected a direct path and the later side's time to direct.
func hardLabCase(t *testing.T, kindA, kindB string, ips bool) (bool, time.Duration) {
	kinds := [2]string{kindA, kindB}
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
		external := fmt.Sprintf("198.18.0.%d", 20+i)
		clients[i], err = l.Namespace("peer")
		check(t, err)
		cfg[i] = labConfig{Advertise: external + ":4000", Hard: true, Observers: observers}
		if kinds[i] == "public" {
			check(t, wan.Attach(clients[i], "eth0", external+"/24", ""))
			continue
		}
		router, e := l.Namespace("router")
		check(t, e)
		check(t, wan.Attach(router, "wan", external+"/24", ""))
		lan, e := router.Segment("lan", fmt.Sprintf("10.%d.0.1/24", i))
		check(t, e)
		inside := fmt.Sprintf("10.%d.0.2", i)
		check(t, lan.Attach(clients[i], "eth0", inside+"/24", fmt.Sprintf("10.%d.0.1", i)))
		allocation, filter, _ := strings.Cut(kinds[i], "-")
		if allocation == "eim" {
			hardLabNAT(t, router, inside, external, "eim", filter)
		} else {
			hardLabNAT(t, router, inside, external, allocation, "apdf")
		}
		if kinds[i] == "sequential" {
			cfg[i].Noise = 200 * time.Millisecond
		}
	}
	peers := startLabPair(t, clients, cfg)
	for i, p := range peers {
		r := p.call(t, labCommand{Op: "profile"}).Report
		t.Logf("peer%d (%s) measured: %s; last allocation=%v", i, kinds[i], r.Profile, r.IPv4.Samples)
		if !eimLike(kinds[i]) && r.Profile.Mapping != netcheck.MappingAddressPortDependent {
			t.Fatalf("fixture did not measure APDM: %s", r.Profile)
		}
		if eimLike(kinds[i]) && !eim(r.Profile) {
			t.Fatalf("fixture did not measure EIM: %s", r.Profile)
		}
		if kinds[i] == "sequential" && r.Profile.Allocation != netcheck.AllocSequential {
			t.Fatalf("fixture did not measure sequential allocation: %s", r.Profile)
		}
		if kinds[i] == "random" && r.Profile.Allocation != netcheck.AllocRandom {
			t.Fatalf("fixture did not measure random allocation: %s", r.Profile)
		}
	}
	if ips {
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
		// Past the 3 s cheap-method wait with no attempt negotiated: none will start.
		if time.Since(start) > 5*time.Second && a.HardAttempts == 0 && b.HardAttempts == 0 {
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
	switch {
	case kinds[0] == "random" && kinds[1] == "random":
		if a.HardSent != 0 || b.HardSent != 0 || a.HardAttempts != 0 || b.HardAttempts != 0 {
			t.Fatal("both-random must stay relayed")
		}
	case !eimLike(kinds[0]) || !eimLike(kinds[1]):
		if a.HardAttempts != 1 || b.HardAttempts != 1 {
			t.Fatal("extension did not negotiate an attempt", a, b)
		}
	}
	if ips && (success || a.HardBackoffs != 1 || b.HardBackoffs != 1 || a.HardSockets != 0 || b.HardSockets != 0) {
		t.Fatal("IPS-like drop did not cancel/back off and release sockets", a, b)
	}
	// Whether direct or relayed, the same inner connection must still work.
	peers[0].call(t, labCommand{Op: "transfer", Bytes: 64 << 10})
	r := peers[0].call(t, labCommand{Op: "result"})
	if r.Progress != 64<<10 || !r.Alive {
		t.Fatal("inner transfer did not survive", r)
	}
	if !success {
		return false, 0
	}
	return true, max(a.TimeToDirect, b.TimeToDirect)
}
