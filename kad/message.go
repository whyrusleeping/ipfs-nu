package kad

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/whyrusleeping/nu/core/varint"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Amino's /ipfs/kad/1.0.0 message enum. Only routing/provider operations are
// implemented; value/IPNS operations are intentionally out of scope.
type MessageType int32

const (
	PutValue     MessageType = 0
	GetValue     MessageType = 1
	AddProvider  MessageType = 2
	GetProviders MessageType = 3
	FindNode     MessageType = 4
	Ping         MessageType = 5
)

var ErrMessage = errors.New("kad: malformed message")

// MaxMessage bounds messages this node sends and requests it serves.
const MaxMessage = 64 << 10

// MaxResponse bounds a response this node reads: GET_PROVIDERS answers for popular keys
// exceed MaxMessage (upstream accepts 4 MiB).
const MaxResponse = 1 << 20
const MaxKey = 80 // upstream go-libp2p Amino rejects provider keys longer than 80 bytes
const MaxPeers = 20
const MaxAddrs = 32

// MaxProviders caps the provider entries kept from one response; further entries (and
// closer peers beyond MaxPeers) are skipped, not an error.
const MaxProviders = 64

// maxAddrBytes bounds one multiaddr.
const maxAddrBytes = 1024

type Peer struct {
	ID    wire.PeerID
	Addrs [][]byte
}
type Message struct {
	Type              MessageType
	Key               []byte
	Closer, Providers []Peer
	ClusterLevel      int32
}

// Encode emits the deployed Kademlia protobuf wire format. Unknown fields are
// not represented; limits are checked before producing the frame.
func Encode(m Message) ([]byte, error) {
	if m.Type < PutValue || m.Type > Ping || len(m.Key) > MaxKey || len(m.Closer) > MaxPeers || len(m.Providers) > MaxPeers {
		return nil, wire.ErrLimit
	}
	var b []byte
	b = uintField(b, 1, uint64(m.Type))
	b = bytesField(b, 2, m.Key)
	for _, p := range m.Closer {
		x, e := encodePeer(p)
		if e != nil {
			return nil, e
		}
		b = bytesField(b, 8, x)
	}
	for _, p := range m.Providers {
		x, e := encodePeer(p)
		if e != nil {
			return nil, e
		}
		b = bytesField(b, 9, x)
	}
	b = uintField(b, 10, uint64(m.ClusterLevel))
	if len(b) > MaxMessage {
		return nil, wire.ErrLimit
	}
	return b, nil
}
func encodePeer(p Peer) ([]byte, error) {
	if _, e := wire.PeerIDFromBytes(p.ID.Bytes()); e != nil {
		return nil, e
	}
	if len(p.Addrs) > MaxAddrs {
		return nil, wire.ErrLimit
	}
	b := bytesField(nil, 1, p.ID.Bytes())
	for _, a := range p.Addrs {
		if len(a) > 1024 {
			return nil, wire.ErrLimit
		}
		b = bytesField(b, 2, a)
	}
	return b, nil
}
func uintField(b []byte, n int, v uint64) []byte {
	b = varint.Append(b, uint64(n<<3))
	return binary.AppendUvarint(b, v)
}
func bytesField(b []byte, n int, v []byte) []byte {
	b = varint.Append(b, uint64(n<<3|2))
	b = varint.Append(b, uint64(len(v)))
	return append(b, v...)
}

type parser struct {
	b   []byte
	off int
}

func (p *parser) vi() (uint64, error) {
	if p.off >= len(p.b) {
		return 0, ErrMessage
	}
	v, n, e := varint.Decode(p.b[p.off:])
	if e != nil {
		return 0, e
	}
	p.off += n
	return v, nil
}
func (p *parser) value() (uint64, error) {
	if p.off >= len(p.b) {
		return 0, ErrMessage
	}
	v, n := binary.Uvarint(p.b[p.off:])
	if n <= 0 || n != len(binary.AppendUvarint(nil, v)) {
		return 0, ErrMessage
	}
	p.off += n
	return v, nil
}
func (p *parser) data(max int) ([]byte, error) {
	n, e := p.vi()
	if e != nil {
		return nil, e
	}
	if n > uint64(max) || n > uint64(len(p.b)-p.off) {
		return nil, wire.ErrLimit
	}
	x := append([]byte(nil), p.b[p.off:p.off+int(n)]...)
	p.off += int(n)
	return x, nil
}
func (p *parser) skip(w int) error {
	switch w {
	case 0:
		_, e := p.value()
		return e
	case 1:
		if len(p.b)-p.off < 8 {
			return ErrMessage
		}
		p.off += 8
		return nil
	case 2:
		_, e := p.data(len(p.b))
		return e
	case 5:
		if len(p.b)-p.off < 4 {
			return ErrMessage
		}
		p.off += 4
		return nil
	}
	return ErrMessage
}

// decodeResponsePeer is decodePeer for responses: addresses over maxAddrBytes or beyond
// MaxAddrs are dropped instead of failing the entry.
func decodeResponsePeer(b []byte) (Peer, error) {
	p := parser{b: b}
	var out Peer
	for p.off < len(p.b) {
		t, e := p.vi()
		if e != nil {
			return out, e
		}
		n, w := int(t>>3), int(t&7)
		if n == 2 && w == 2 {
			x, e := p.data(len(p.b))
			if e != nil {
				return out, e
			}
			if len(x) <= maxAddrBytes && len(out.Addrs) < MaxAddrs {
				out.Addrs = append(out.Addrs, x)
			}
			continue
		}
		if n == 1 && w == 2 {
			x, e := p.data(44)
			if e != nil {
				return out, e
			}
			if out.ID, e = wire.PeerIDFromBytes(x); e != nil {
				return out, e
			}
		} else if e = p.skip(w); e != nil {
			return out, e
		}
	}
	if out.ID == "" {
		return out, ErrMessage
	}
	return out, nil
}

func decodePeer(b []byte) (Peer, error) {
	p := parser{b: b}
	var out Peer
	for p.off < len(p.b) {
		t, e := p.vi()
		if e != nil {
			return out, e
		}
		n, w := int(t>>3), int(t&7)
		if n == 1 && w == 2 {
			x, e := p.data(44)
			if e != nil {
				return out, e
			}
			out.ID, e = wire.PeerIDFromBytes(x)
			if e != nil {
				return out, e
			}
		} else if n == 2 && w == 2 {
			if len(out.Addrs) >= MaxAddrs {
				return out, wire.ErrLimit
			}
			x, e := p.data(1024)
			if e != nil {
				return out, e
			}
			out.Addrs = append(out.Addrs, x)
		} else if e = p.skip(w); e != nil {
			return out, e
		}
	}
	if out.ID == "" {
		return out, ErrMessage
	}
	return out, nil
}

// Decode parses an Amino request, skips supported protobuf unknown fields and
// rejects oversized/repeated collections before allocation.
func Decode(b []byte) (Message, error) { return decode(b, false) }

// DecodeResponse parses an Amino response of at most MaxResponse bytes. It keeps the first
// MaxPeers valid closer peers and MaxProviders valid providers and skips the rest; an
// entry with a bad peer ID is dropped, and addresses beyond the per-peer bounds are
// dropped. Protobuf framing errors still fail the message.
func DecodeResponse(b []byte) (Message, error) { return decode(b, true) }

func decode(b []byte, resp bool) (Message, error) {
	var m Message
	limit := MaxMessage
	if resp {
		limit = MaxResponse
	}
	if len(b) > limit {
		return m, wire.ErrLimit
	}
	p := parser{b: b}
	for p.off < len(p.b) {
		tag, e := p.vi()
		if e != nil {
			return m, e
		}
		n, w := int(tag>>3), int(tag&7)
		if n == 0 {
			return m, ErrMessage
		}
		switch n {
		case 1:
			if w != 0 {
				return m, ErrMessage
			}
			v, e := p.value()
			if e != nil {
				return m, e
			}
			if v > uint64(Ping) {
				return m, ErrMessage
			}
			m.Type = MessageType(v)
		case 2:
			if w != 2 {
				return m, ErrMessage
			}
			m.Key, e = p.data(MaxKey)
			if e != nil {
				return m, e
			}
		case 8, 9:
			if w != 2 {
				return m, ErrMessage
			}
			x, e := p.data(limit)
			if e != nil {
				return m, e
			}
			list, limitN := &m.Closer, MaxPeers
			if n == 9 {
				list = &m.Providers
				if resp {
					limitN = MaxProviders
				}
			}
			if !resp {
				peer, e := decodePeer(x)
				if e != nil {
					return m, e
				}
				if len(*list) >= limitN {
					return m, wire.ErrLimit
				}
				*list = append(*list, peer)
			} else if len(*list) < limitN {
				if peer, e := decodeResponsePeer(x); e == nil {
					*list = append(*list, peer)
				}
			}
		case 10:
			if w != 0 {
				return m, ErrMessage
			}
			v, e := p.value()
			if e != nil {
				return m, e
			}
			m.ClusterLevel = int32(uint32(v))
		default:
			if e := p.skip(w); e != nil {
				return m, e
			}
		}
	}
	if m.Type < PutValue || m.Type > Ping {
		return m, fmt.Errorf("%w: message type", ErrMessage)
	}
	return m, nil
}
