package swarm

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/whyrusleeping/nu/p2p/wire"
)

// Private/isolated cluster members find each other by cluster-derived mDNS (0069); a
// node of another cluster is not found, and without a cluster ID mDNS stays off.
func TestClusterMDNS(t *testing.T) {
	iface := loopbackMulticast(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := func(clusterID string) *Swarm {
		k, err := wire.GenerateNodeKey()
		if err != nil {
			t.Fatal(err)
		}
		s, err := Start(ctx, Config{Key: k, Listen: []string{"/ip4/127.0.0.1/udp/0/quic-v1"}, MDNS: true,
			MDNSInterfaces: []net.Interface{iface}, Isolated: true, MDNSCluster: clusterID, Logger: discard})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	a, b, other, off := start("bafyclusterA"), start("bafyclusterA"), start("bafyclusterB"), start("")
	c, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	if err := a.Connect(c, b.ID(), nil); err != nil {
		t.Fatalf("member via cluster mDNS: %v", err)
	}
	for _, s := range []*Swarm{other, off} {
		c, stop := context.WithTimeout(ctx, 600*time.Millisecond)
		if err := a.Connect(c, s.ID(), nil); err == nil {
			t.Fatalf("found %s outside the cluster's mDNS service", s.ID())
		}
		stop()
	}
}

// loopbackMulticast returns the loopback interface when the OS loops multicast on it.
func loopbackMulticast(t *testing.T) net.Interface {
	t.Helper()
	iface, err := net.InterfaceByName("lo")
	if err != nil {
		t.Skip("no lo interface")
	}
	group := &net.UDPAddr{IP: net.ParseIP("224.0.0.251")}
	c, err := net.ListenMulticastUDP("udp4", iface, group)
	if err != nil {
		t.Skip("multicast unavailable:", err)
	}
	defer c.Close()
	p := ipv4.NewPacketConn(c)
	if p.SetMulticastInterface(iface) != nil || p.SetMulticastLoopback(true) != nil {
		t.Skip("multicast loopback unavailable")
	}
	group.Port = c.LocalAddr().(*net.UDPAddr).Port
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := c.WriteToUDP([]byte("probe"), group); err != nil {
		t.Skip("loopback multicast send:", err)
	}
	var buf [16]byte
	if n, _, err := c.ReadFromUDP(buf[:]); err != nil || string(buf[:n]) != "probe" {
		t.Skip("OS does not loop multicast on loopback")
	}
	return *iface
}
