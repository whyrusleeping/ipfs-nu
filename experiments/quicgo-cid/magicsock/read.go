package magicsock

import (
	"bytes"
	"errors"
	"net"
	"os"
	"time"

	"golang.org/x/net/ipv4"
)

func (c *Conn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, _, _, a, err := c.ReadMsgUDP(b, nil)
	return n, a, err
}
func (c *Conn) ReadMsgUDP(b, oob []byte) (int, int, int, *net.UDPAddr, error) {
	// Preserve virtual source demux even when the caller requests fewer bytes
	// than a CID header. The hot quic-go path calls ReadBatch directly.
	buf := b
	if len(buf) < 2048 {
		buf = make([]byte, 2048)
	}
	ms := [1]ipv4.Message{{Buffers: [][]byte{buf}, OOB: oob}}
	_, err := c.ReadBatch(ms[:], 0)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	m := &ms[0]
	return copy(b, buf[:m.N]), m.NN, m.Flags, m.Addr.(*net.UDPAddr), nil
}

// ReadBatch is the sole physical receive loop, driven by quic-go. Normal UDP
// batches land directly in its packet buffers: no intermediate copy, per-packet
// pool or channel hand-off. Relay arrivals interrupt the fd read using a PRIVATE
// deadline, then are copied from the bounded relay queue. The logical caller
// deadline is unchanged. readMu ensures a wake/reset cannot race another reader.
func (c *Conn) ReadBatch(ms []ipv4.Message, flags int) (int, error) {
	if len(ms) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return 0, net.ErrClosed
		}
		if c.readErr != nil {
			err := c.readErr
			c.mu.Unlock()
			return 0, err
		}
		if !c.readDeadline.IsZero() && !c.readDeadline.After(time.Now()) {
			c.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		}
		// Checking the queue and arming the fd read share mu with deliver's wakeup.
		// Enqueue-before-arm and enqueue-after-arm therefore both wake the reader.
		if len(c.incoming) > 0 {
			c.mu.Unlock()
			n := 0
			for n < len(ms) {
				select {
				case d := <-c.incoming:
					copyDatagram(&ms[n], d)
					n++
				default:
					return n, nil
				}
			}
			return n, nil
		}
		c.readingUDP = true
		_ = c.udp.SetReadDeadline(c.readDeadline)
		c.mu.Unlock()
		n, err := c.packetUDP.ReadBatch(ms, flags)
		c.mu.Lock()
		c.readingUDP = false
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				c.mu.Unlock()
				continue
			} // relay wake or logical timeout; check at loop top
			if c.readErr == nil {
				c.readErr = err
				close(c.readDone)
			}
			c.mu.Unlock()
			return 0, err
		}
		c.mu.Unlock()
		if out := c.routeBatch(ms[:n]); out > 0 {
			return out, nil
		}
	}
}

func copyDatagram(m *ipv4.Message, d datagram) {
	m.N = 0
	b := d.b
	for _, dst := range m.Buffers {
		n := copy(dst, b)
		m.N += n
		b = b[n:]
		if len(b) == 0 {
			break
		}
	}
	m.NN = copy(m.OOB, d.oob)
	m.Addr = d.addr
	m.Flags = d.flags
}
func messageBytes(m *ipv4.Message) []byte {
	if len(m.Buffers) == 1 {
		return m.Buffers[0][:m.N]
	}
	// Scatter receive is unusual (quic-go always supplies one buffer), but retain
	// its semantics rather than assuming headers fit inside the first iovec.
	b := make([]byte, m.N)
	off := 0
	for _, src := range m.Buffers {
		off += copy(b[off:], src)
		if off == len(b) {
			break
		}
	}
	return b
}
func (c *Conn) routeBatch(ms []ipv4.Message) int {
	out := 0
	var lastPeer *Peer
	var lastAddr *net.UDPAddr
	for i := range ms {
		m := &ms[i]
		b := messageBytes(m)
		if len(b) >= 4 && bytes.Equal(b[:4], discoMagic[:4]) {
			payload, err := DecodeDisco(b)
			if err != nil {
				c.dropped.Add(1)
				continue
			}
			select {
			case c.disco <- datagram{b: append([]byte(nil), payload...), addr: m.Addr}:
			default:
				c.dropped.Add(1)
			}
			continue
		}
		source, ok := addrPort(m.Addr)
		if !ok {
			continue
		}
		if cid, ok := destinationCID(b); ok {
			c.mu.Lock()
			r, exists := c.cids[cid]
			if exists {
				if !r.peer.direct[source] {
					c.mu.Unlock()
					c.dropped.Add(1)
					continue
				}
				if lastPeer != r.peer {
					lastPeer = r.peer
					lastAddr = net.UDPAddrFromAddrPort(r.peer.addr)
				}
				m.Addr = lastAddr
				m.NN = 0
			}
			c.mu.Unlock()
		}
		// Never swap message buffers: quic-go's separate packetBuffer bookkeeping
		// owns them by index. Copy only across holes left by filtered/probe packets.
		if out != i {
			copyDatagram(&ms[out], datagram{b: b, oob: m.OOB[:m.NN], addr: m.Addr, flags: m.Flags})
		}
		out++
	}
	c.received.Add(uint64(out))
	return out
}
