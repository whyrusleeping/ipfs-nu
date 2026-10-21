//go:build linux && natlab

package qpath_test

import (
	"context"
	"flag"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/natlab"
	"golang.org/x/sys/unix"
)

var socketBind = flag.String("qpath-socket-bind", "", "namespace socket helper bind")
var socketFD = flag.String("qpath-socket-fd", "", "namespace socket helper fd channel")

func TestSocketHelper(t *testing.T) {
	if *socketBind == "" {
		return
	}
	u := udp(t, *socketBind)
	file, err := u.File()
	check(t, err)
	defer file.Close()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: *socketFD, Net: "unix"})
	check(t, err)
	defer c.Close()
	_, _, err = c.WriteMsgUnix([]byte{1}, unix.UnixRights(int(file.Fd())), nil)
	check(t, err)
}

// SCM_RIGHTS preserves each socket's namespace routing without setns on Go
// threads. All links/rules remain inside Isolate's disposable network namespace.
func namespaceUDP(t *testing.T, n *natlab.Namespace, bind string) *net.UDPConn {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fd")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	check(t, err)
	defer l.Close()
	check(t, l.SetDeadline(time.Now().Add(3*time.Second)))
	cmd := n.Command(os.Args[0], "-test.run=^TestSocketHelper$", "-qpath-socket-bind="+bind, "-qpath-socket-fd="+path)
	cmd.Stderr = os.Stderr
	check(t, cmd.Start())
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	c, err := l.AcceptUnix()
	check(t, err)
	defer c.Close()
	check(t, c.SetReadDeadline(time.Now().Add(3*time.Second)))
	data := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := c.ReadMsgUnix(data, oob)
	check(t, err)
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	check(t, err)
	if len(msgs) != 1 {
		t.Fatal("no fd")
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	check(t, err)
	if len(fds) != 1 {
		t.Fatal("wrong fd count")
	}
	file := os.NewFile(uintptr(fds[0]), "qpath-natlab")
	pc, err := net.FilePacketConn(file)
	_ = file.Close()
	check(t, err)
	u := pc.(*net.UDPConn)
	t.Cleanup(func() { u.Close() })
	return u
}
func TestQpathNAT(t *testing.T) {
	if !natlab.Isolate(t) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	lab, err := natlab.New(ctx)
	check(t, err)
	t.Cleanup(lab.Close)
	public, err := lab.Namespace("public")
	check(t, err)
	wan, err := public.Segment("wan", "198.18.0.1/24")
	check(t, err)
	var peers, routers [2]*natlab.Namespace
	for i := range peers {
		routers[i], err = lab.Namespace("router")
		check(t, err)
		peers[i], err = lab.Namespace("peer")
		check(t, err)
		external := "198.18.0.20"
		inside := "10.1.0.2:4000"
		gateway := "10.1.0.1"
		peerCIDR := "10.1.0.2/24"
		other := "198.18.0.30:4000"
		if i == 1 {
			external = "198.18.0.30"
			inside = "10.2.0.2:4000"
			gateway = "10.2.0.1"
			peerCIDR = "10.2.0.2/24"
			other = "198.18.0.20:4000"
		}
		check(t, wan.Attach(routers[i], "wan", external+"/24", ""))
		lan, err := routers[i].Segment("lan", gateway+"/24")
		check(t, err)
		check(t, lan.Attach(peers[i], "eth0", peerCIDR, gateway))
		_, err = routers[i].NAT(natlab.NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr(external), Mapping: natlab.EndpointIndependent, Filtering: natlab.FilterAddressPortDependent, Inside: []netip.AddrPort{netip.MustParseAddrPort(inside)}, Destinations: []netip.AddrPort{netip.MustParseAddrPort("198.18.0.1:4000"), netip.MustParseAddrPort(other)}})
		check(t, err)
	}
	x := makeSetup(t, ctx, namespaceUDP(t, peers[0], "10.1.0.2:4000"), namespaceUDP(t, peers[1], "10.2.0.2:4000"), namespaceUDP(t, public, "198.18.0.1:4000"))
	s := x.stream(t, ctx)
	roundtrip(t, s, 4)
	before, ok := x.a.Connection(x.b.ID())
	if !ok {
		t.Fatal("missing inner")
	}
	aa, ba := netip.MustParseAddrPort("198.18.0.20:4000"), netip.MustParseAddrPort("198.18.0.30:4000")
	// Outer relay traffic has activated EIM mappings. Simultaneous disposable
	// probes open APDF filters; 0017 replaces this script with authenticated disco.
	for range 3 {
		check(t, x.ma.WriteDisco([]byte("fixture"), ba))
		check(t, x.mb.WriteDisco([]byte("fixture"), aa))
		time.Sleep(time.Millisecond)
	}
	buf := make([]byte, 1024)
	_, from, err := x.ma.ReadDisco(ctx, buf)
	check(t, err)
	if from != ba {
		t.Fatal("not NAT translated", from)
	}
	_, from, err = x.mb.ReadDisco(ctx, buf)
	check(t, err)
	if from != aa {
		t.Fatal("not NAT translated", from)
	}
	start := time.Now()
	x.selectDirect(t, aa, ba)
	roundtrip(t, s, 8)
	t.Logf("one shared UDP socket per private host: direct verified after %s", time.Since(start))
	check(t, routers[0].NFT("add table inet broken\nadd chain inet broken forward { type filter hook forward priority -10; policy accept; }\nadd rule inet broken forward ip daddr 198.18.0.30 udp dport 4000 drop\n"))
	check(t, x.ma.UseRelay(x.b.ID()))
	check(t, x.mb.UseRelay(x.a.ID()))
	roundtrip(t, s, 4)
	check(t, routers[0].Link("wan", false))
	time.Sleep(20 * time.Millisecond)
	check(t, routers[0].Link("wan", true))
	roundtrip(t, s, 2)
	after, ok := x.a.Connection(x.b.ID())
	if !ok || after != before {
		t.Fatal("inner connection replaced")
	}
	if x.ma.Stats().DirectSent == 0 || x.mb.Stats().DirectSent == 0 {
		t.Fatal("direct not used")
	}
	t.Logf("one authenticated inner stream survived relay/direct/blocked direct/relay/WAN flap: A=%+v B=%+v", x.ma.Stats(), x.mb.Stats())
}
