//go:build linux && natlab

package disco

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/natlab"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func startLabPair(t *testing.T, clients [2]*natlab.Namespace, cfg [2]labConfig) [2]*labPeer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay")
	listener, err := net.Listen("unix", path)
	check(t, err)
	defer listener.Close()
	keys := []*wire.NodeKey{key(t), key(t)}
	var peers [2]*labPeer
	for i := range peers {
		in, out, err := clients[i].Start("env", "NU_DISCO_HELPER=1", os.Args[0], "-test.run=^TestDiscoLabHelper$")
		check(t, err)
		peers[i] = &labPeer{in, json.NewDecoder(out)}
		cfg[i].Key = keys[i].Marshal()
		cfg[i].Peer = keys[1-i].Public().PeerID().String()
		cfg[i].Relay = path
		check(t, json.NewEncoder(in).Encode(cfg[i]))
	}
	left, err := listener.Accept()
	check(t, err)
	right, err := listener.Accept()
	check(t, err)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(left, right); done <- struct{}{} }()
	go func() { _, _ = io.Copy(right, left); done <- struct{}{} }()
	t.Cleanup(func() { left.Close(); right.Close(); <-done; <-done })
	for _, p := range peers {
		var r labReply
		check(t, p.dec.Decode(&r))
		if !r.Alive {
			t.Fatal("helper not ready")
		}
	}
	return peers
}
func assertLabDirect(t *testing.T, peers [2]*labPeer, scenario string) {
	t.Helper()
	start := time.Now()
	peers[0].call(t, labCommand{Op: "open"})
	eventually(t, func() bool {
		return peers[0].call(t, labCommand{Op: "status"}).Status.Selected.IsValid() && peers[1].call(t, labCommand{Op: "status"}).Status.Selected.IsValid()
	})
	t.Logf("%s: both direct in %v; peer0=%s peer1=%s", scenario, time.Since(start), peers[0].call(t, labCommand{Op: "status"}).Status.Selected, peers[1].call(t, labCommand{Op: "status"}).Status.Selected)
	peers[0].call(t, labCommand{Op: "transfer"})
	r := peers[0].call(t, labCommand{Op: "result"})
	if r.Progress != 4<<20 || !r.Alive {
		t.Fatal("verified direct transfer failed", r)
	}
}
func TestNATLabPublicEIM(t *testing.T) {
	if !natlab.Isolate(t) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	l, err := natlab.New(ctx)
	check(t, err)
	defer l.Close()
	internet, err := l.Namespace("internet")
	check(t, err)
	wan, err := internet.Segment("br0", "198.18.0.1/24")
	check(t, err)
	public, err := l.Namespace("public")
	check(t, err)
	check(t, wan.Attach(public, "eth0", "198.18.0.30/24", ""))
	router, err := l.Namespace("router")
	check(t, err)
	check(t, wan.Attach(router, "wan", "198.18.0.20/24", ""))
	lan, err := router.Segment("lan", "10.0.0.1/24")
	check(t, err)
	private, err := l.Namespace("private")
	check(t, err)
	check(t, lan.Attach(private, "eth0", "10.0.0.2/24", "10.0.0.1"))
	_, err = router.NAT(natlab.NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr("198.18.0.20"), Inside: []netip.AddrPort{netip.MustParseAddrPort("10.0.0.2:4000")}, Destinations: []netip.AddrPort{netip.MustParseAddrPort("198.18.0.30:4000")}, Mapping: natlab.EndpointIndependent, Filtering: natlab.FilterAddressPortDependent})
	check(t, err)
	peers := startLabPair(t, [2]*natlab.Namespace{public, private}, [2]labConfig{{Advertise: "198.18.0.30:4000"}, {Advertise: "198.18.0.20:4000"}})
	assertLabDirect(t, peers, "public IPv4 <-> EIM/APDF")
}
func TestNATLabIPv6(t *testing.T) {
	if !natlab.Isolate(t) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	l, err := natlab.New(ctx)
	check(t, err)
	defer l.Close()
	internet, err := l.Namespace("internet6")
	check(t, err)
	wan, err := internet.Segment("br0", "2001:db8:1::1/64")
	check(t, err)
	var clients [2]*natlab.Namespace
	var cfg [2]labConfig
	for i := range clients {
		clients[i], err = l.Namespace(fmt.Sprintf("peer%d", i))
		check(t, err)
		_, err = clients[i].Run("sysctl", "-qw", "net.ipv6.conf.all.accept_dad=0", "net.ipv6.conf.default.accept_dad=0")
		check(t, err)
		ip := fmt.Sprintf("2001:db8:1::%d", 10+i)
		check(t, wan.Attach(clients[i], "eth0", ip+"/64", ""))
		cfg[i] = labConfig{Advertise: "[" + ip + "]:4000", Listen: "[::]:4000", Scope: "2001:db8:1::/64"}
	}
	assertLabDirect(t, startLabPair(t, clients, cfg), "IPv6 <-> IPv6, no NAT")
}
func TestNATLabMappedMiniUPnPd(t *testing.T) {
	binary := os.Getenv("NU_MINIUPNPD")
	if binary == "" {
		var err error
		binary, err = exec.LookPath("miniupnpd")
		if err != nil {
			t.Skip("install miniupnpd-nftables or set NU_MINIUPNPD for real mapped-path acceptance")
		}
	}
	if !natlab.Isolate(t) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	l, err := natlab.New(ctx)
	check(t, err)
	defer l.Close()
	internet, err := l.Namespace("internet")
	check(t, err)
	wan, err := internet.Segment("br0", "11.0.0.1/24")
	check(t, err)
	router, err := l.Namespace("mapped-router")
	check(t, err)
	check(t, wan.Attach(router, "wan", "11.0.0.20/24", ""))
	lan, err := router.Segment("lan", "10.0.0.1/24")
	check(t, err)
	mapped, err := l.Namespace("mapped-peer")
	check(t, err)
	check(t, lan.Attach(mapped, "eth0", "10.0.0.2/24", "10.0.0.1"))
	otherRouter, err := l.Namespace("eim-router")
	check(t, err)
	check(t, wan.Attach(otherRouter, "wan", "11.0.0.21/24", ""))
	otherLAN, err := otherRouter.Segment("lan", "10.1.0.1/24")
	check(t, err)
	public, err := l.Namespace("eim-peer")
	check(t, err)
	check(t, otherLAN.Attach(public, "eth0", "10.1.0.2/24", "10.1.0.1"))
	_, err = otherRouter.NAT(natlab.NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr("11.0.0.21"), Inside: []netip.AddrPort{netip.MustParseAddrPort("10.1.0.2:4000")}, Destinations: []netip.AddrPort{netip.MustParseAddrPort("11.0.0.20:4000")}, Mapping: natlab.EndpointIndependent, Filtering: natlab.FilterAddressPortDependent})
	check(t, err)
	_, err = router.Run("sysctl", "-qw", "net.ipv4.ip_forward=1")
	check(t, err)
	check(t, router.NFT(`table ip nu_mapped {
 chain forward { type filter hook forward priority filter; policy drop; ct state established,related accept; iifname "lan" accept; ct status dnat accept; }
 chain outbound { type nat hook postrouting priority srcnat; policy accept; oifname "wan" masquerade; }
}`))
	dir := t.TempDir()
	conf := filepath.Join(dir, "miniupnpd.conf")
	check(t, os.WriteFile(conf, []byte("ext_ifname=wan\next_ip=11.0.0.20\nlistening_ip=lan\nhttp_port=5000\nlease_file="+filepath.Join(dir, "leases")+"\nminissdpdsocket="+filepath.Join(dir, "minissdpd.sock")+"\nenable_natpmp=yes\nenable_upnp=yes\nsecure_mode=yes\nsystem_uptime=no\nuuid=25fe7acc-798b-4219-b2f9-45b74a288c43\nallow 1024-65535 10.0.0.0/24 1024-65535\ndeny 0-65535 0.0.0.0/0 0-65535\n"), 0600))
	version, err := router.Run(binary, "--version")
	check(t, err)
	t.Logf("real mapping daemon: %s", version)
	_, _, err = router.Start(binary, "-d", "-4", "-f", conf, "-P", filepath.Join(dir, "pid"))
	check(t, err)
	peers := startLabPair(t, [2]*natlab.Namespace{mapped, public}, [2]labConfig{{Advertise: "11.0.0.20:4000", Scope: "11.0.0.0/24", Gateway: "10.0.0.1", Internal: "10.0.0.2:4000"}, {Advertise: "11.0.0.21:4000", Scope: "11.0.0.0/24"}})
	m := peers[0].call(t, labCommand{Op: "map"}).Mapping
	if !m.External.IsValid() || m.Internal != netip.MustParseAddrPort("10.0.0.2:4000") {
		t.Fatal("missing real mapping", m)
	}
	rules, err := router.Run("nft", "list", "ruleset")
	check(t, err)
	if !strings.Contains(string(rules), "dnat to 10.0.0.2:4000") {
		t.Fatal("miniupnpd did not install actual mapping", string(rules))
	}
	t.Logf("portmap UPnP lease: %s -> %s, expires %s", m.Internal, m.External, m.Expires)
	assertLabDirect(t, peers, "real miniupnpd mapped side <-> EIM/APDF")
}
