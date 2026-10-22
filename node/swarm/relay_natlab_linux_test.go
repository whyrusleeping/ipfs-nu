//go:build linux && natlab

package swarm_test

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/natlab"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Pair-aware relay choice with a peer relay (0019), real `nu` binaries: two
// daemons behind EIM/APDF NATs, a distant project relay (40 ms one-way netem on
// its interface, ~80 ms per leg) and a public seed daemon on the same WAN
// segment offering `swarm.peer_relay_allow` for exactly these two peers. Both
// daemons list the far relay first. After the relayed inner connection comes up
// the lower peer fetches the other's /nu/relays/1 table and re-homes the pair
// onto the seed; status on both daemons must show the seed as the relay with
// the datagram leg negotiated, while the inner connection survives the switch.
func TestDaemonPairAwarePeerRelay(t *testing.T) {
	if !natlab.IsolateTimeout(t, 90*time.Second) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	t.Cleanup(cancel)
	bin := filepath.Join(t.TempDir(), "nu")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "../../cmd/nu")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	lab, err := natlab.New(ctx)
	labCheck(t, err)
	t.Cleanup(lab.Close)
	internet, err := lab.Namespace("internet")
	labCheck(t, err)
	wan, err := internet.Segment("wan", "198.18.0.1/24")
	labCheck(t, err)
	far, err := lab.Namespace("relay")
	labCheck(t, err)
	labCheck(t, wan.Attach(far, "eth0", "198.18.0.2/24", ""))
	seedNS, err := lab.Namespace("seed")
	labCheck(t, err)
	labCheck(t, wan.Attach(seedNS, "eth0", "198.18.0.5/24", ""))
	_, err = far.Run("tc", "qdisc", "add", "dev", "eth0", "root", "netem", "delay", "40ms")
	labCheck(t, err)
	var clients, routers [2]*natlab.Namespace
	for i := range clients {
		routers[i], err = lab.Namespace("router")
		labCheck(t, err)
		clients[i], err = lab.Namespace("daemon")
		labCheck(t, err)
		labCheck(t, wan.Attach(routers[i], "wan", fmt.Sprintf("198.18.0.%d/24", 20+10*i), ""))
		lan, err := routers[i].Segment("lan", fmt.Sprintf("10.%d.0.1/24", i+1))
		labCheck(t, err)
		labCheck(t, lan.Attach(clients[i], "eth0", fmt.Sprintf("10.%d.0.2/24", i+1), fmt.Sprintf("10.%d.0.1", i+1)))
		_, err = routers[i].NAT(natlab.NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr(fmt.Sprintf("198.18.0.%d", 20+10*i)), Mapping: natlab.EndpointIndependent, Filtering: natlab.FilterAddressPortDependent, Inside: []netip.AddrPort{netip.MustParseAddrPort(fmt.Sprintf("10.%d.0.2:4171", i+1))}, Destinations: []netip.AddrPort{netip.MustParseAddrPort("198.18.0.2:4000"), netip.MustParseAddrPort("198.18.0.5:4171")}})
		labCheck(t, err)
	}
	var dirs [2]string
	var keys [2]*wire.NodeKey
	for i := range dirs {
		dirs[i] = filepath.Join(t.TempDir(), "node")
		labRun(t, clients[i], bin, dirs[i], "init")
		keys[i], err = wire.LoadNodeKey(filepath.Join(dirs[i], repo.KeyFile))
		labCheck(t, err)
	}
	rdir, sdir := filepath.Join(t.TempDir(), "relay"), filepath.Join(t.TempDir(), "seed")
	labRun(t, far, bin, rdir, "init")
	labRun(t, seedNS, bin, sdir, "init")
	rkey, err := wire.LoadNodeKey(filepath.Join(rdir, repo.KeyFile))
	labCheck(t, err)
	skey, err := wire.LoadNodeKey(filepath.Join(sdir, repo.KeyFile))
	labCheck(t, err)
	admit := []string{keys[0].Public().PeerID().String(), keys[1].Public().PeerID().String()}
	labStart(t, far, bin, rdir, "relay", "--listen", "198.18.0.2:4000", "--admit", admit[0], "--admit", admit[1])
	labConfig(t, sdir, "swarm.listen", labArray("/ip4/198.18.0.5/udp/4171/quic-v1"))
	labConfig(t, sdir, "swarm.mdns", "false")
	labConfig(t, sdir, "swarm.peer_relay_allow", labArray(admit...))
	labConfig(t, sdir, "api.listen", "127.0.0.1:0")
	labConfig(t, sdir, "gateway.listen", "127.0.0.1:0")
	seed := labStart(t, seedNS, bin, sdir, "daemon")
	relays := []string{
		fmt.Sprintf("/ip4/198.18.0.2/udp/4000/quic-v1/p2p/%s", rkey.Public().PeerID()),
		fmt.Sprintf("/ip4/198.18.0.5/udp/4171/quic-v1/p2p/%s", skey.Public().PeerID()),
	}
	var daemons [2]*daemonLabProcess
	for i := range clients {
		labConfig(t, dirs[i], "swarm.listen", labArray(fmt.Sprintf("/ip4/10.%d.0.2/udp/4171/quic-v1", i+1)))
		labConfig(t, dirs[i], "swarm.mdns", "false")
		labConfig(t, dirs[i], "swarm.relays", labArray(relays...))
		labConfig(t, dirs[i], "api.listen", "127.0.0.1:0")
		labConfig(t, dirs[i], "gateway.listen", "127.0.0.1:0")
		daemons[i] = labStart(t, clients[i], bin, dirs[i], "daemon")
	}
	labWait(t, ctx, "daemons reserving home and backup (far relay + seed)", func() bool {
		select {
		case <-seed.done:
			t.Fatalf("seed exited: %v", seed.err)
		default:
		}
		for i := range clients {
			s := labStatus(clients[i], bin, dirs[i])
			if s == nil || s.Connectivity == nil || s.Connectivity.Home == "" || s.Connectivity.Backup == "" {
				return false
			}
		}
		return true
	})
	provider := labStatus(clients[0], bin, dirs[0])
	t.Logf("provider relays: home=%s backup=%s (far=%s seed=%s)", provider.Connectivity.Home, provider.Connectivity.Backup, rkey.Public().PeerID().Base36(), skey.Public().PeerID().Base36())
	// Connect through the far relay explicitly: a share link whose only relay
	// hint is the far relay forces the initial leg there.
	input := filepath.Join(t.TempDir(), "input.bin")
	labCheck(t, os.WriteFile(input, bytes.Repeat([]byte("pair-aware relay fixture\n"), 8000), 0600))
	root := labAdd(t, clients[0], bin, dirs[0], input)
	farHint := fmt.Sprintf("%s/p2p-circuit/p2p/%s", relays[0], keys[0].Public().PeerID())
	link := fmt.Sprintf("nu://%s?peer=%s&addr=%s", root, keys[0].Public().PeerID().Base36(), url.QueryEscape(farHint))
	start := time.Now()
	labRun(t, clients[1], bin, dirs[1], "get", "-o", filepath.Join(t.TempDir(), "copy.bin"), link)
	t.Logf("get through far relay hint completed in %v", time.Since(start))
	low, high := 0, 1
	if keys[1].Public().PeerID() < keys[0].Public().PeerID() {
		low, high = 1, 0
	}
	labWait(t, ctx, "pair re-homed onto the seed peer relay", func() bool {
		a := labPath(clients[low], bin, dirs[low], keys[high].Public().PeerID().Base36())
		b := labPath(clients[high], bin, dirs[high], keys[low].Public().PeerID().Base36())
		return a.Path == "relay" && a.Relay == skey.Public().PeerID().Base36() && b.Relay == skey.Public().PeerID().Base36() && a.RelayMode == "datagram"
	})
	a := labPath(clients[low], bin, dirs[low], keys[high].Public().PeerID().Base36())
	logged, err := os.ReadFile(daemons[low].log)
	labCheck(t, err)
	want := fmt.Sprintf("from=%s to=%s", rkey.Public().PeerID().Base36(), skey.Public().PeerID().Base36())
	if !bytes.Contains(logged, []byte("pair-aware relay switch")) || !bytes.Contains(logged, []byte(want)) {
		t.Fatalf("initiator did not log a far→seed switch (%s): %s", want, logged)
	}
	t.Logf("pair-aware choice moved the pair from the 40 ms-netem project relay to the LAN-region seed in %v: %+v", time.Since(start), a)
}
