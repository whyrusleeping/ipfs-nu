package magicsock

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func socket(t *testing.T) *net.UDPConn {
	t.Helper()
	u, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	check(t, err)
	t.Cleanup(func() { u.Close() })
	return u
}
func peerID(t *testing.T) wire.PeerID {
	t.Helper()
	k, err := wire.GenerateNodeKey()
	check(t, err)
	return k.Public().PeerID()
}
func TestBoundsDemuxDeadlines(t *testing.T) {
	c, err := New(Config{UDP: socket(t), MaxPeers: 1, RelayQueue: 1})
	check(t, err)
	defer c.Close()
	id := peerID(t)
	addr, err := c.AddPeer(id)
	check(t, err)
	if _, err = c.AddPeer(peerID(t)); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if !errors.Is(c.VerifyPeerAddress(net.UDPAddrFromAddrPort(addr), peerID(t)), ErrPeer) {
		t.Fatal("virtual identity was not bound")
	}
	check(t, c.SetReadDeadline(time.Now().Add(time.Millisecond)))
	_, _, err = c.ReadFrom(make([]byte, 1500))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal(err)
	}
	check(t, c.SetReadDeadline(time.Time{}))
	// A recognizable inner CID from an unauthorized source must not turn into a
	// QUIC-visible migration; unrelated physical traffic still passes unchanged.
	c.mu.Lock()
	p := c.peers[id]
	key, _ := destinationCID([]byte("\x40abcdefgh"))
	c.cids[key] = cidRoute{peer: p}
	c.mu.Unlock()
	sender := socket(t)
	_, err = sender.WriteToUDP([]byte("\x40abcdefghgarbage"), c.LocalAddr().(*net.UDPAddr))
	check(t, err)
	payload := []byte("\x40unknown!raw")
	_, err = sender.WriteToUDP(payload, c.LocalAddr().(*net.UDPAddr))
	check(t, err)
	check(t, c.SetReadDeadline(time.Now().Add(time.Second)))
	buf := make([]byte, 1500)
	n, from, err := c.ReadFrom(buf)
	check(t, err)
	if !bytes.Equal(buf[:n], payload) || from.String() != sender.LocalAddr().String() {
		t.Fatal("physical packet was rewritten")
	}
	if c.Stats().Dropped == 0 {
		t.Fatal("unauthorized CID packet was not dropped")
	}
	// Replacing an active relay leg requires the owner's explicit close.
	a, b := net.Pipe()
	defer b.Close()
	r, err := c.AttachRelay(id, a)
	check(t, err)
	otherA, otherB := net.Pipe()
	defer otherA.Close()
	defer otherB.Close()
	if _, err = c.AttachRelay(id, otherA); !errors.Is(err, ErrRelayExists) {
		t.Fatal(err)
	}
	packet := make([]byte, 1200)
	packet[0] = 0x40
	for range 128 {
		_, err = c.WriteTo(packet, net.UDPAddrFromAddrPort(addr))
		check(t, err)
	}
	if c.Stats().Dropped < 100 {
		t.Fatal("relay did not enforce bounded backlog")
	}
	check(t, r.Close())
	c.RemovePeer(id)
	next, err := c.AddPeer(id)
	check(t, err)
	if next == addr {
		t.Fatal("virtual address reused")
	}
	if _, err = c.WriteTo(packet, net.UDPAddrFromAddrPort(addr)); !errors.Is(err, ErrPeer) {
		t.Fatal("removed virtual address escaped to UDP", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = c.ReadDisco(ctx, buf)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func FuzzFrames(f *testing.F) {
	for _, b := range [][]byte{{0, 1, 0x40}, {0, 0}, {4, 177}, {0, 3, 0x40, 1}, {1, 'n', 'u', 'd', 1, 0, 1, 42}} {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if packet, err := ReadFrame(bytes.NewReader(b)); err == nil {
			if len(packet) > PacketSize {
				t.Fatal("oversize")
			}
			var out bytes.Buffer
			check(t, WriteFrame(&out, packet))
			if !bytes.Equal(out.Bytes(), b[:len(out.Bytes())]) {
				t.Fatal("frame did not round trip")
			}
		}
		if payload, err := DecodeDisco(b); err == nil {
			out, err := EncodeDisco(payload)
			check(t, err)
			if !bytes.Equal(b, out) {
				t.Fatal("disco did not round trip")
			}
		}
		_, _ = destinationCID(b)
	})
}

// A malformed relay frame kills only that leg, never the logical socket.
func TestMalformedRelay(t *testing.T) {
	c, err := New(Config{UDP: socket(t)})
	check(t, err)
	defer c.Close()
	id := peerID(t)
	_, err = c.AddPeer(id)
	check(t, err)
	a, b := net.Pipe()
	defer b.Close()
	r, err := c.AttachRelay(id, a)
	check(t, err)
	_, err = b.Write([]byte{4, 177})
	check(t, err)
	select {
	case <-r.Done():
	case <-time.After(time.Second):
		t.Fatal("bad frame not rejected")
	}
	spare, remote := net.Pipe()
	defer spare.Close()
	defer remote.Close()
	if _, err = c.AttachRelay(id, spare); !errors.Is(err, ErrRelayExists) {
		t.Fatal("failed workers' slot released before joining")
	}
	check(t, r.Close())
	_, err = c.AttachRelay(id, spare)
	check(t, err)
	if _, err = ReadFrame(bytes.NewReader([]byte{0, 2, 0x40})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
}
