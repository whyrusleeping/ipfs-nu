package magicsock

import (
	"bytes"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestAuxiliarySocketAdmission(t *testing.T) {
	c, err := New(Config{UDP: socket(t)})
	check(t, err)
	defer c.Close()
	id := peerID(t)
	virtual, err := c.AddPeer(id)
	check(t, err)
	aux, remote := socket(t), socket(t)
	from := remote.LocalAddr().(*net.UDPAddr).AddrPort()
	if c.UseDirectSocket(id, from, aux) == nil {
		t.Fatal("unproven endpoint selected")
	}
	check(t, c.SetDirectPaths(id, []netip.AddrPort{from}))
	check(t, c.UseDirectSocket(id, from, aux))
	packet := []byte("\x40abcdefghpayload")
	cid, _ := destinationCID(packet)
	c.mu.Lock()
	c.cids[cid] = cidRoute{peer: c.peers[id]}
	c.mu.Unlock()
	c.InjectDirectSocket(id, remote, from, packet)
	c.InjectDirectSocket(peerID(t), aux, from, packet)
	c.InjectDirectSocket(id, aux, from, []byte("\x40unknown!payload"))
	if len(c.incoming) != 0 {
		t.Fatal("wrong socket/peer/CID admitted")
	}
	c.InjectDirectSocket(id, aux, from, packet)
	check(t, c.SetReadDeadline(time.Now().Add(time.Second)))
	buf := make([]byte, 1500)
	n, addr, err := c.ReadFrom(buf)
	check(t, err)
	if !bytes.Equal(buf[:n], packet) || addr.String() != virtual.String() {
		t.Fatal("auxiliary receive demux")
	}
	_, err = c.WriteTo(packet, net.UDPAddrFromAddrPort(virtual))
	check(t, err)
	check(t, remote.SetReadDeadline(time.Now().Add(time.Second)))
	n, addr, err = remote.ReadFrom(buf)
	check(t, err)
	if addr.String() != aux.LocalAddr().String() || !bytes.Equal(buf[:n], packet) {
		t.Fatal("auxiliary send did not preserve its NAT tuple")
	}
	check(t, c.SetDirectPaths(id, nil))
	c.InjectDirectSocket(id, aux, from, packet)
	if len(c.incoming) != 0 {
		t.Fatal("revoked auxiliary path admitted")
	}
}
