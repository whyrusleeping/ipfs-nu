package wire

import (
	"bytes"
	"fmt"

	ma "github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/nu/core/varint"
)

// Multiaddr retains exact binary bytes. Registered protocols outside Nu's
// transport subset are carried without assigning them transport semantics.
// At an unregistered protocol code the remaining bytes are opaque: multiaddr
// has no self-describing length for an unknown protocol, so splitting that
// suffix is impossible without its registry entry. Such addresses are never dialed.
type Multiaddr struct {
	raw    []byte
	opaque bool
}

func ParseMultiaddr(s string, maxBytes int) (Multiaddr, error) {
	// Text can be longer than binary (e.g. a textual IPv6 address).
	if maxBytes < 0 || len(s) > maxBytes {
		return Multiaddr{}, ErrLimit
	}
	addr, err := ma.NewMultiaddr(s)
	if err != nil {
		return Multiaddr{}, err
	}
	return MultiaddrFromBytes(addr.Bytes(), maxBytes)
}
func MultiaddrFromBytes(b []byte, maxBytes int) (Multiaddr, error) {
	if maxBytes < 0 || len(b) > maxBytes {
		return Multiaddr{}, ErrLimit
	}
	rest := b
	opaque := false
	for len(rest) > 0 {
		code, n, err := varint.Decode(rest)
		if err != nil {
			return Multiaddr{}, err
		}
		rest = rest[n:]
		if code == 0 {
			return Multiaddr{}, ErrFormat
		}
		if code > uint64(^uint(0)>>1) {
			return Multiaddr{}, ErrFormat
		}
		p := ma.ProtocolWithCode(int(code))
		if p.Code == 0 {
			opaque = true
			break
		}
		size := uint64(p.Size / 8)
		if p.Size < 0 {
			size, n, err = varint.Decode(rest)
			if err != nil {
				return Multiaddr{}, err
			}
			rest = rest[n:]
		}
		if size > uint64(len(rest)) {
			return Multiaddr{}, ErrFormat
		}
		rest = rest[int(size):]
	}
	if !opaque {
		if _, err := ma.NewMultiaddrBytes(b); err != nil {
			return Multiaddr{}, err
		}
	}
	return Multiaddr{raw: bytes.Clone(b), opaque: opaque}, nil
}
func (a Multiaddr) Bytes() []byte { return bytes.Clone(a.raw) }
func (a Multiaddr) Opaque() bool  { return a.opaque }

// Text returns a standard multiaddr string, or an error for unregistered codes.
// Bytes always round-trips, even when Text is unavailable.
func (a Multiaddr) Text() (string, error) {
	if a.opaque {
		return "", fmt.Errorf("wire: unregistered multiaddr protocol")
	}
	m, err := ma.NewMultiaddrBytes(a.raw)
	if err != nil {
		return "", err
	}
	return m.String(), nil
}
func (a Multiaddr) String() string {
	s, err := a.Text()
	if err != nil {
		return fmt.Sprintf("<opaque multiaddr %x>", a.raw)
	}
	return s
}
