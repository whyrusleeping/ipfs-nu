//go:build linux && natlab

package swarm_test

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/nutest/slow"
	"github.com/whyrusleeping/nu/p2p/natlab"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// TestMagicPreviewMovement isolates scenario 3 from the expensive 10 GB/tree
// checks. No daemon restart or CLI retry is allowed, including in failure cases.
func TestMagicPreviewMovement(t *testing.T) {
	slow.Skip(t)
	if !natlab.IsolateTimeout(t, 15*time.Minute) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()
	bin := labBinary(t, ctx)
	for _, which := range []string{"recipient", "provider", "both"} {
		t.Run(which, func(t *testing.T) { movementCase(t, bin, which) })
	}
}

func movementCase(t *testing.T, bin, which string) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	lab, err := natlab.New(ctx)
	labCheck(t, err)
	t.Cleanup(lab.Close)
	internet, err := lab.Namespace("internet")
	labCheck(t, err)
	wan, err := internet.Segment("wan", "198.18.0.1/24")
	labCheck(t, err)
	relay, err := lab.Namespace("relay")
	labCheck(t, err)
	labCheck(t, wan.Attach(relay, "eth0", "198.18.0.2/24", ""))
	var ns [2]*natlab.Namespace
	var dirs [2]string
	var keys [2]*wire.NodeKey
	for i := range ns {
		ns[i], err = lab.Namespace("laptop")
		labCheck(t, err)
		for j := range 2 {
			router, e := lab.Namespace("router")
			labCheck(t, e)
			external := fmt.Sprintf("198.18.0.%d", 20+i+70*j)
			labCheck(t, wan.Attach(router, "wan", external+"/24", ""))
			subnet := i + 1 + 97*j
			gateway := fmt.Sprintf("10.%d.0.1", subnet)
			inside := fmt.Sprintf("10.%d.0.2", subnet)
			lan, e := router.Segment("lan", gateway+"/24")
			labCheck(t, e)
			gw := ""
			if j == 0 {
				gw = gateway
			}
			iface := fmt.Sprintf("eth%d", j)
			labCheck(t, lan.Attach(ns[i], iface, inside+"/24", gw))
			mapping, filter := natlab.EndpointIndependent, natlab.FilterEndpointIndependent
			if j == 1 {
				labCheck(t, ns[i].Link(iface, false))
				mapping, filter = natlab.AddressPortDependent, natlab.FilterAddressPortDependent
			}
			_, e = router.NAT(natlab.NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr(external), Mapping: mapping, Filtering: filter, Inside: []netip.AddrPort{netip.MustParseAddrPort(inside + ":4171")}, Destinations: []netip.AddrPort{netip.MustParseAddrPort("198.18.0.2:4000")}})
			labCheck(t, e)
		}
		dirs[i] = filepath.Join(t.TempDir(), "repo")
		labRun(t, ns[i], bin, dirs[i], "init")
		keys[i], err = wire.LoadNodeKey(filepath.Join(dirs[i], repo.KeyFile))
		labCheck(t, err)
	}
	rdir := filepath.Join(t.TempDir(), "relay")
	labRun(t, relay, bin, rdir, "init")
	rk, err := wire.LoadNodeKey(filepath.Join(rdir, repo.KeyFile))
	labCheck(t, err)
	args := []string{"relay", "--listen", "198.18.0.2:4000"}
	for _, k := range keys {
		args = append(args, "--admit", k.Public().PeerID().String())
	}
	labStart(t, relay, bin, rdir, args...)
	for i := range ns {
		labConfig(t, dirs[i], "swarm.relays", labArray("/ip4/198.18.0.2/udp/4000/quic-v1/p2p/"+rk.Public().PeerID().String()))
		labStart(t, ns[i], bin, dirs[i], "daemon")
		labWait(t, ctx, "relay reservation", func() bool {
			s := labStatus(ns[i], bin, dirs[i])
			return s != nil && s.Connectivity != nil && s.Connectivity.Home != ""
		})
	}
	const size = 512 << 20
	input := filepath.Join(t.TempDir(), "moving.bin")
	f, err := os.Create(input)
	labCheck(t, err)
	labCheck(t, f.Truncate(size))
	_, err = f.WriteAt([]byte("0029 separate Wi-Fi to hotspot transfer"), size/2)
	labCheck(t, err)
	labCheck(t, f.Close())
	digest := magicDigest(t, input, size)
	root := magicAdd(t, ns[0], bin, dirs[0], "--raw", "--nocopy", input)
	shared := magicShare(t, ns[0], bin, dirs[0], root)
	_, err = ns[0].Run("tc", "qdisc", "add", "dev", "eth0", "root", "tbf", "rate", "64mbit", "burst", "64kb", "latency", "500ms")
	labCheck(t, err)
	output := filepath.Join(t.TempDir(), "received.bin")
	var changed time.Time
	var retained, last int64
	start := time.Now()
	err = magicGet(t, ns[1], bin, dirs[1], output, shared.String(), func(done int64) {
		if done < last {
			t.Fatalf("verified progress regressed: %d < %d", done, last)
		}
		last = done
		if !changed.IsZero() || done < size/4 || done >= size {
			return
		}
		retained, changed = done, time.Now()
		for i := range ns {
			if which == "recipient" && i != 1 || which == "provider" && i != 0 {
				continue
			}
			labCheck(t, ns[i].Link("eth0", false))
			labCheck(t, ns[i].Link("eth1", true))
			_, e := ns[i].Run("ip", "route", "replace", "default", "via", fmt.Sprintf("10.%d.0.1", i+98), "dev", "eth1")
			labCheck(t, e)
		}
		_, e := ns[0].Run("tc", "qdisc", "del", "dev", "eth0", "root")
		labCheck(t, e)
		t.Logf("%s interface/subnet/WAN switch at %d verified bytes", which, retained)
	})
	labCheck(t, err)
	if changed.IsZero() || last < retained {
		t.Fatal("no in-progress network change demonstrated")
	}
	if magicDigest(t, output, size) != digest {
		t.Fatal("download SHA-256 mismatch")
	}
	t.Logf("PASS %s: %d bytes SHA-256=%x; elapsed=%v after-switch=%v; no daemon restart or get retry", which, size, digest, time.Since(start), time.Since(changed))
}
