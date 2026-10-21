package magicsock

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/netip"

	"github.com/whyrusleeping/nu/p2p/wire"
)

var discoMagic = []byte{1, 'n', 'u', 'd', 1}

const MaxDiscoPayload = 1024

// Disco framing is ONLY a bounded socket-demux envelope. The payload must carry
// the authenticated nonce/MAC/destination contract supplied by 0017; this package
// never responds to it, authorizes a candidate, or changes paths on receipt.
// Wire: 01 6e 75 64 01, uint16 big-endian payload length, then 1..1024 bytes.
func EncodeDisco(payload []byte) ([]byte, error) {
	if len(payload) < 1 || len(payload) > MaxDiscoPayload {
		return nil, wire.ErrLimit
	}
	b := make([]byte, 7+len(payload))
	copy(b, discoMagic)
	binary.BigEndian.PutUint16(b[5:7], uint16(len(payload)))
	copy(b[7:], payload)
	return b, nil
}
func DecodeDisco(b []byte) ([]byte, error) {
	if len(b) < 7 || !bytes.Equal(b[:5], discoMagic) {
		return nil, wire.ErrFormat
	}
	n := int(binary.BigEndian.Uint16(b[5:7]))
	if n < 1 || n > MaxDiscoPayload {
		return nil, wire.ErrLimit
	}
	if len(b) != 7+n {
		return nil, wire.ErrFormat
	}
	return b[7:], nil
}
func (c *Conn) WriteDisco(payload []byte, dest netip.AddrPort) error {
	if !dest.IsValid() || dest.Port() == 0 || IsVirtual(dest) || dest.Addr().IsUnspecified() || dest.Addr().IsMulticast() {
		return wire.ErrFormat
	}
	b, err := EncodeDisco(payload)
	if err != nil {
		return err
	}
	_, err = c.WriteTo(b, net.UDPAddrFromAddrPort(dest))
	return err
}
func (c *Conn) ReadDisco(ctx context.Context, b []byte) (int, netip.AddrPort, error) {
	select {
	case <-ctx.Done():
		return 0, netip.AddrPort{}, ctx.Err()
	case <-c.done:
		return 0, netip.AddrPort{}, net.ErrClosed
	case <-c.readDone:
		c.mu.Lock()
		err := c.readErr
		c.mu.Unlock()
		return 0, netip.AddrPort{}, err
	case d := <-c.disco:
		if len(b) < len(d.b) {
			return 0, netip.AddrPort{}, wire.ErrLimit
		}
		a, _ := addrPort(d.addr)
		return copy(b, d.b), a, nil
	}
}
