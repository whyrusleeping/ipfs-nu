//go:build linux && natlab

package cluster_test

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/node/link"
	"github.com/whyrusleeping/nu/nutest"
	"github.com/whyrusleeping/nu/p2p/natlab"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func sh(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return string(out)
}

// An isolated cluster sends nothing to non-member addresses and no DNS queries
// (docs/cluster.md "Egress safety"). The test runs in a fresh network namespace with a
// default route out of a dummy interface, so any egress attempt would leave a packet;
// nftables counts every packet to a non-loopback address and every DNS query. Members
// listen on loopback; a member is tempted with a share link naming a non-member.
func TestIsolatedNoEgress(t *testing.T) {
	if !natlab.Isolate(t) {
		return
	}
	sh(t, "ip", "link", "set", "lo", "up")
	sh(t, "ip", "link", "add", "egress0", "type", "dummy")
	sh(t, "ip", "addr", "add", "10.99.0.1/24", "dev", "egress0")
	sh(t, "ip", "link", "set", "egress0", "up")
	sh(t, "ip", "route", "add", "default", "dev", "egress0")
	sh(t, "nft", "add table inet egress")
	sh(t, "nft", "add chain inet egress out { type filter hook output priority 0 ; }")
	sh(t, "nft", "add rule inet egress out ip daddr != 127.0.0.0/8 counter comment \"nonmember\"")
	sh(t, "nft", "add rule inet egress out ip6 daddr != ::1 counter comment \"nonmember6\"")
	sh(t, "nft", "add rule inet egress out udp dport 53 counter comment \"dns\"")
	sh(t, "nft", "add rule inet egress out tcp dport 53 counter comment \"dnstcp\"")

	n := nutest.New(t, 3)
	form(t, n, cluster.ModeIsolated)
	a := n.Nodes[0]

	// A share link naming a non-member provider must not make a member dial it.
	outsider, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := wire.ParseMultiaddr("/ip4/10.99.0.2/udp/4171/quic-v1", 1024)
	fctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c := n.Nodes[1].AddBytes(n.RandBytes(1024))
	_ = a.Local.Fetcher.Fetch(fctx, c, []link.Provider{{Peer: outsider.Public().PeerID(), Addrs: []wire.Multiaddr{addr}}}, nil)
	// Several heartbeat/anti-entropy rounds.
	eventually(t, "heartbeat rounds", func() bool {
		p := a.Local.ClusterService().Peers()[n.Nodes[2].ID]
		return p.Heartbeat != nil && p.Heartbeat.Seq >= 5
	})

	counters := sh(t, "nft", "list table inet egress")
	re := regexp.MustCompile(`packets (\d+) bytes \d+ comment "(\w+)"`)
	m := re.FindAllStringSubmatch(counters, -1)
	if len(m) != 4 {
		t.Fatalf("cannot read counters:\n%s", counters)
	}
	for _, c := range m {
		if p, _ := strconv.Atoi(c[1]); p != 0 {
			t.Errorf("%s packets: %d\n%s", c[2], p, counters)
		}
	}
	// The members did talk (over loopback).
	if !a.ConnectedTo(n.Nodes[1].ID) {
		t.Fatal("members are not connected")
	}
}
