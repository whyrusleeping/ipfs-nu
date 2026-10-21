package magicsock

import (
	"net"
	"os"
	"syscall"
	"time"

	quic "github.com/quic-go/quic-go"
)

// A comparable inline CID avoids converting every UDP header into a heap string.
type cidKey = quic.ConnectionID

func destinationCID(b []byte) (cidKey, bool) {
	if len(b) < 1 || b[0]&0x40 == 0 {
		return cidKey{}, false
	}
	if b[0]&0x80 != 0 {
		if len(b) < 6 {
			return cidKey{}, false
		}
		n := int(b[5])
		if n == 0 || n > 20 || len(b) < 6+n {
			return cidKey{}, false
		}
		return quic.ConnectionIDFromBytes(b[6 : 6+n]), true
	}
	if len(b) < 1+ConnectionIDLength {
		return cidKey{}, false
	}
	return quic.ConnectionIDFromBytes(b[1 : 1+ConnectionIDLength]), true
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
