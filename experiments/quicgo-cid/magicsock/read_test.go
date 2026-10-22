package magicsock

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

func TestReadBatchOwnershipAndWake(t *testing.T) {
	c, err := New(Config{UDP: socket(t)})
	check(t, err)
	defer c.Close()
	id := peerID(t)
	virtual, err := c.AddPeer(id)
	check(t, err)
	source := socket(t)
	check(t, c.SetDirectPaths(id, []netip.AddrPort{source.LocalAddr().(*net.UDPAddr).AddrPort()}))
	payload := []byte("\x40abcdefghverified packet")
	cid, _ := destinationCID(payload)
	c.mu.Lock()
	c.cids[cid] = cidRoute{peer: c.peers[id]}
	c.mu.Unlock()
	probe, err := EncodeDisco([]byte("probe"))
	check(t, err)
	_, err = source.WriteTo(probe, c.LocalAddr())
	check(t, err)
	_, err = source.WriteTo(payload, c.LocalAddr())
	check(t, err)
	first, second := make([]byte, 1500), make([]byte, 1500)
	ms := [2]ipv4.Message{{Buffers: [][]byte{first}, OOB: make([]byte, 128)}, {Buffers: [][]byte{second}, OOB: make([]byte, 128)}}
	check(t, c.SetReadDeadline(time.Now().Add(time.Second)))
	n, err := c.ReadBatch(ms[:], 0)
	check(t, err)
	if n != 1 || !bytes.Equal(ms[0].Buffers[0][:ms[0].N], payload) || &ms[0].Buffers[0][0] != &first[0] || &ms[1].Buffers[0][0] != &second[0] {
		t.Fatal("filtered batch changed caller buffer ownership")
	}
	if ms[0].Addr.String() != virtual.String() {
		t.Fatal("lost virtual source", ms[0].Addr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, from, err := c.ReadDisco(ctx, make([]byte, 1024))
	check(t, err)
	if from != source.LocalAddr().(*net.UDPAddr).AddrPort() {
		t.Fatal("lost physical probe source")
	}
	// Wait for the sole reader to enter an idle physical fd read; a relay arrival
	// must wake it without expiring/changing the caller's logical deadline.
	logical := time.Now().Add(time.Second)
	check(t, c.SetReadDeadline(logical))
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 1500)
		n, addr, err := c.ReadFrom(buf)
		if err == nil && (n != len(payload) || addr.String() != virtual.String() || !bytes.Equal(buf[:n], payload)) {
			err = fmt.Errorf("wrong injected packet")
		}
		done <- err
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		c.mu.Lock()
		reading := c.readingUDP
		c.mu.Unlock()
		if reading {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("reader did not block")
		}
	}
	c.deliver(datagram{b: payload, addr: net.UDPAddrFromAddrPort(virtual)})
	select {
	case err := <-done:
		check(t, err)
	case <-ctx.Done():
		t.Fatal("relay did not interrupt fd read")
	}
	c.mu.Lock()
	same := c.readDeadline.Equal(logical)
	c.mu.Unlock()
	if !same {
		t.Fatal("private wake changed public deadline")
	}
	// Reset the private wake on the next physical read. A truncated public read
	// still returns the virtual source, even though its buffer cannot hold a CID.
	_, err = source.WriteTo(payload, c.LocalAddr())
	check(t, err)
	buf := make([]byte, 1)
	n, addr, err := c.ReadFrom(buf)
	check(t, err)
	if n != 1 || addr.String() != virtual.String() {
		t.Fatal("wake reset or small-buffer demux failed")
	}
}
