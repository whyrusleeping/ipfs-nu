//go:build linux && natlab

package nupath

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

var socketBind = flag.String("nupath-bind", "", "namespace socket helper bind")
var socketFD = flag.String("nupath-fd", "", "namespace socket helper fd channel")

// Only socket creation occurs in the subprocess namespace. SCM_RIGHTS keeps the
// socket's original network namespace, routing and conntrack in the parent Go
// process. No setns on Go scheduler threads and no production hook is needed.
func TestSocketHelper(t *testing.T) {
	if *socketBind == "" {
		return
	}
	addr, err := net.ResolveUDPAddr("udp4", *socketBind)
	check(t, err)
	udp, err := net.ListenUDP("udp4", addr)
	check(t, err)
	defer udp.Close()
	file, err := udp.File()
	check(t, err)
	defer file.Close()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: *socketFD, Net: "unix"})
	check(t, err)
	defer c.Close()
	_, _, err = c.WriteMsgUnix([]byte{1}, unix.UnixRights(int(file.Fd())), nil)
	check(t, err)
}
func namespaceUDP(t *testing.T, n *natlab.Namespace, bind string) *net.UDPConn {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fd")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	check(t, err)
	defer listener.Close()
	check(t, listener.SetDeadline(time.Now().Add(3*time.Second)))
	cmd := n.Command(os.Args[0], "-test.run=^TestSocketHelper$", "-nupath-bind="+bind, "-nupath-fd="+path)
	cmd.Stderr = os.Stderr
	check(t, cmd.Start())
	c, err := listener.AcceptUnix()
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
	file := os.NewFile(uintptr(fds[0]), "natlab-udp")
	pc, err := net.FilePacketConn(file)
	_ = file.Close()
	check(t, err)
	check(t, cmd.Wait())
	udp := pc.(*net.UDPConn)
	t.Cleanup(func() { _ = udp.Close() })
	return udp
}
func TestNATPaths(t *testing.T) {
	if !natlab.Isolate(t) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	lab, err := natlab.New(ctx)
	check(t, err)
	t.Cleanup(lab.Close)
	public, err := lab.Namespace("public")
	check(t, err)
	wan, err := public.Segment("wan", "198.18.0.1/24")
	check(t, err)
	var peers [2]*natlab.Namespace
	var routers [2]*natlab.Namespace
	for i := range peers {
		routers[i], err = lab.Namespace("router")
		check(t, err)
		peers[i], err = lab.Namespace("private")
		check(t, err)
		outside := "198.18.0.20"
		inside := "10.1.0.2:5000"
		routerCIDR := "10.1.0.1/24"
		peerCIDR := "10.1.0.2/24"
		gateway := "10.1.0.1"
		other := "198.18.0.30:5000"
		if i == 1 {
			outside = "198.18.0.30"
			inside = "10.2.0.2:5000"
			routerCIDR = "10.2.0.1/24"
			peerCIDR = "10.2.0.2/24"
			gateway = "10.2.0.1"
			other = "198.18.0.20:5000"
		}
		check(t, wan.Attach(routers[i], "wan", outside+"/24", ""))
		lan, err := routers[i].Segment("lan", routerCIDR)
		check(t, err)
		check(t, lan.Attach(peers[i], "eth0", peerCIDR, gateway))
		inner := netip.MustParseAddrPort(inside)
		outer := netip.AddrPortFrom(inner.Addr(), 4000)
		_, err = routers[i].NAT(natlab.NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr(outside), Mapping: natlab.EndpointIndependent, Filtering: natlab.FilterAddressPortDependent, Inside: []netip.AddrPort{inner, outer}, Destinations: []netip.AddrPort{netip.MustParseAddrPort("198.18.0.1:4000"), netip.MustParseAddrPort(other)}})
		check(t, err)
	}
	a := packetConnFromUDP(namespaceUDP(t, peers[0], "10.1.0.2:5000"))
	b := packetConnFromUDP(namespaceUDP(t, peers[1], "10.2.0.2:5000"))
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	a.peer, err = net.ResolveUDPAddr("udp4", "198.18.0.30:5000")
	check(t, err)
	b.peer, err = net.ResolveUDPAddr("udp4", "198.18.0.20:5000")
	check(t, err)
	factory := circuitFactory(t, ctx, namespaceUDP(t, public, "198.18.0.1:4000"), namespaceUDP(t, peers[0], "10.1.0.2:4000"), namespaceUDP(t, peers[1], "10.2.0.2:4000"))
	ra, rb := factory()
	a.attach(ra)
	b.attach(rb)
	// Simultaneous probes open APDF pinholes; connect verifies the observed PUBLIC
	// source on the shared socket. These are unauthenticated disposable test probes.
	observer := &net.UDPAddr{IP: net.IPv4(198, 18, 0, 1), Port: 4999}
	_, err = a.udp.WriteTo([]byte("\x01observe"), observer)
	check(t, err)
	_, err = b.udp.WriteTo([]byte("\x01observe"), observer)
	check(t, err)
	time.Sleep(10 * time.Millisecond)
	for range 3 {
		_, err = a.udp.WriteTo([]byte("\x01disco"), a.peer)
		check(t, err)
		_, err = b.udp.WriteTo([]byte("\x01disco"), b.peer)
		check(t, err)
		time.Sleep(time.Millisecond)
	}
	begin := time.Now()
	p := connect(t, ctx, a, b, true)
	x := startTransfer(ctx, p)
	defer x.finish()
	progress(t, ctx, x, 512<<10)
	t.Logf("NAT relay first512KiB=%s", time.Since(begin))
	a.direct.Store(true)
	b.direct.Store(true)
	start := time.Now()
	progress(t, ctx, x, 1<<20)
	t.Logf("NAT direct1MiB=%s, public probe sources verified", time.Since(start))
	// Kernel firewall blocks the selected direct pair; relay traffic is unaffected.
	check(t, routers[0].NFT("add table inet broken\nadd chain inet broken forward { type filter hook forward priority -10; policy accept; }\nadd rule inet broken forward udp dport 5000 drop\n"))
	start = time.Now()
	time.Sleep(100 * time.Millisecond)
	a.direct.Store(false)
	b.direct.Store(false)
	progress(t, ctx, x, 512<<10)
	t.Logf("NAT broken direct -> relay 512KiB=%s", time.Since(start))
	a.detach()
	b.detach()
	check(t, routers[0].Link("wan", false))
	time.Sleep(100 * time.Millisecond)
	check(t, routers[0].Link("wan", true))
	ra, rb = factory()
	a.attach(ra)
	b.attach(rb)
	progress(t, ctx, x, 512<<10)
	x.finish()
	select {
	case err := <-x.done:
		check(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if a.directPackets.Load() == 0 || a.relayPackets.Load() == 0 {
		t.Fatal("paths unused")
	}
	t.Logf("NAT same inner connection+stream bytes=%d elapsed=%s %s", x.received.Load(), time.Since(begin), p.trace.snapshot())
}
