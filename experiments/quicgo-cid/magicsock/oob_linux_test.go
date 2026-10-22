//go:build linux

package magicsock

import (
	"encoding/binary"
	"net"
	"testing"
	"unsafe"

	quic "github.com/quic-go/quic-go"
	"golang.org/x/sys/unix"
)

var _ quic.OOBCapablePacketConn = (*Conn)(nil)

func TestRelayGSOSplit(t *testing.T) {
	c, err := New(Config{UDP: socket(t)})
	check(t, err)
	defer c.Close()
	id := peerID(t)
	addr, err := c.AddPeer(id)
	check(t, err)
	a, b := net.Pipe()
	defer b.Close()
	r, err := c.AttachRelay(id, a)
	check(t, err)
	defer r.Close()
	oob := make([]byte, unix.CmsgSpace(2))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&oob[0]))
	h.Level = unix.SOL_UDP
	h.Type = unix.UDP_SEGMENT
	h.SetLen(unix.CmsgLen(2))
	binary.NativeEndian.PutUint16(oob[unix.CmsgLen(0):], 1200)
	data := make([]byte, 3000)
	data[0] = 0x40
	data[1200] = 0x40
	data[2400] = 0x40
	n, _, err := c.WriteMsgUDP(data, oob, net.UDPAddrFromAddrPort(addr))
	check(t, err)
	if n != 3000 {
		t.Fatal(n)
	}
	for _, want := range []int{1200, 1200, 600} {
		frame, err := ReadFrame(b)
		check(t, err)
		if len(frame) != want {
			t.Fatal("GSO superpacket not segmented", len(frame), want)
		}
	}
}
