package magicsock

import (
	"bytes"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
)

func destinationCID(b []byte) (string, bool) {
	if len(b) < 1 || b[0]&0x40 == 0 {
		return "", false
	}
	if b[0]&0x80 != 0 {
		if len(b) < 6 {
			return "", false
		}
		n := int(b[5])
		if n == 0 || n > 20 || len(b) < 6+n {
			return "", false
		}
		return string(b[6 : 6+n]), true
	}
	if len(b) < 1+ConnectionIDLength {
		return "", false
	}
	return string(b[1 : 1+ConnectionIDLength]), true
}
func (c *Conn) receiveUDP() {
	defer c.wg.Done()
	pc := ipv4.NewPacketConn(c.udp)
	ms := make([]ipv4.Message, 16)
	for i := range ms {
		ms[i].Buffers = [][]byte{make([]byte, 2048)}
		ms[i].OOB = make([]byte, 128)
	}
	for {
		n, err := pc.ReadBatch(ms, 0)
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			close(c.readDone)
			return
		}
		for i := 0; i < n; i++ {
			m := &ms[i]
			b := m.Buffers[0][:m.N]
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
			a, ok := addrPort(m.Addr)
			if !ok {
				continue
			}
			var peer *Peer
			if cid, ok := destinationCID(b); ok {
				c.mu.Lock()
				route, exists := c.cids[cid]
				if exists {
					if !route.peer.direct[a] {
						c.mu.Unlock()
						c.dropped.Add(1)
						continue
					}
					peer = route.peer
				}
				c.mu.Unlock()
			}
			addr := m.Addr
			oob := m.OOB[:m.NN]
			if peer != nil {
				addr = net.UDPAddrFromAddrPort(peer.addr)
				oob = nil
			}
			buffer := receivePool.Get().(*recvBuffer)
			copy(buffer.b[:], b)
			copy(buffer.oob[:], oob)
			c.deliver(datagram{buffer: buffer, b: buffer.b[:len(b)], oob: buffer.oob[:len(oob)], addr: addr, flags: m.Flags})
		}
	}
}
func (c *Conn) WriteTo(b []byte, a net.Addr) (int, error) {
	u, ok := a.(*net.UDPAddr)
	if !ok {
		return 0, ErrPeer
	}
	n, _, err := c.WriteMsgUDP(b, nil, u)
	return n, err
}
func (c *Conn) WriteMsgUDP(b, oob []byte, a *net.UDPAddr) (int, int, error) {
	addr, ok := addrPort(a)
	if !ok {
		return 0, 0, ErrPeer
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, 0, net.ErrClosed
	}
	if !c.writeDeadline.IsZero() && !c.writeDeadline.After(time.Now()) {
		c.mu.Unlock()
		return 0, 0, os.ErrDeadlineExceeded
	}
	p := c.virtual[addr]
	if p == nil {
		c.mu.Unlock()
		if IsVirtual(addr) {
			return 0, 0, ErrPeer
		}
		return c.udp.WriteMsgUDP(b, oob, a)
	}
	direct, relay := p.selected, p.relay
	c.mu.Unlock()
	filtered, segment, err := innerOOB(oob)
	if err != nil {
		return 0, 0, err
	}
	if segment == 0 {
		segment = len(b)
	}
	if segment <= 0 || segment > PacketSize {
		return 0, 0, syscall.EMSGSIZE
	}
	if direct.IsValid() {
		n, _, err := c.udp.WriteMsgUDP(b, filtered, net.UDPAddrFromAddrPort(direct))
		if err == nil {
			c.directSent.Add(uint64((len(b) + segment - 1) / segment))
		}
		return n, len(oob), err
	}
	if !relayAlive(relay) {
		c.dropped.Add(1)
		return len(b), len(oob), nil
	}
	for off := 0; off < len(b); off += segment {
		end := min(off+segment, len(b))
		data := append([]byte(nil), b[off:end]...)
		select {
		case <-relay.done:
			c.dropped.Add(1)
		case relay.queue <- data:
			c.relayed.Add(1)
		default:
			c.dropped.Add(1)
		}
	}
	return len(b), len(oob), nil
}
