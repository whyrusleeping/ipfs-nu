package magicsock

import (
	"net"
	"net/netip"

	"github.com/whyrusleeping/nu/p2p/wire"
)

// UseDirectSocket selects an already round-trip-validated endpoint on a borrowed
// auxiliary socket. The path owner retains the sole reader and lifetime; revoke
// the path before closing it. Ordinary/outer QUIC always uses the primary socket.
func (c *Conn) UseDirectSocket(id wire.PeerID, remote netip.AddrPort, udp *net.UDPConn) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.peers[id]
	if c.closed || p == nil || !p.direct[remote] || udp == nil {
		return ErrNoPath
	}
	p.selected, p.auxiliary = remote, udp
	return nil
}

// InjectDirectSocket accepts QUIC only from the selected auxiliary socket and
// exact authorized remote endpoint, with a routed CID belonging to that peer.
// It cannot admit new connections, probes, arbitrary sources or outer QUIC.
func (c *Conn) InjectDirectSocket(id wire.PeerID, udp *net.UDPConn, from netip.AddrPort, b []byte) {
	if len(b) == 0 || len(b) > PacketSize {
		return
	}
	cid, ok := destinationCID(b)
	if !ok {
		return
	}
	c.mu.Lock()
	p := c.peers[id]
	r := c.cids[cid]
	if c.closed || p == nil || p.auxiliary != udp || p.selected != from || !p.direct[from] || r.peer != p {
		c.mu.Unlock()
		return
	}
	addr := net.UDPAddrFromAddrPort(p.addr)
	c.mu.Unlock()
	c.receivedBytes.direct.Add(uint64(len(b)))
	c.deliver(datagram{b: append([]byte(nil), b...), addr: addr})
}

func (c *Conn) writeAuxiliary(udp *net.UDPConn, b []byte, remote netip.AddrPort, segment, oobLen int) (int, int, error) {
	for off := 0; off < len(b); off += segment {
		part := b[off:min(off+segment, len(b))]
		n, err := udp.WriteToUDPAddrPort(part, remote)
		if err != nil {
			// The borrowed socket can disappear during path invalidation. As for
			// an unavailable relay, drop rather than killing the inner connection.
			c.dropped.Add(1)
			continue
		}
		c.directSent.Add(1)
		c.sentBytes.direct.Add(uint64(n))
	}
	return len(b), oobLen, nil
}
