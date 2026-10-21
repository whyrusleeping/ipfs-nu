package wire

import (
	"crypto/sha256"
	"strings"

	"github.com/mr-tron/base58/base58"
	mb "github.com/multiformats/go-multibase"
	"github.com/whyrusleeping/nu/core/varint"
)

// PeerID is a comparable binary multihash, not its human-readable encoding.
// The zero value is invalid. Constructors enforce identity <=42 or SHA-256/32.
type PeerID string

func (k PublicKey) PeerID() PeerID {
	b := k.Marshal()
	if len(b) <= 42 {
		return PeerID(append([]byte{0, byte(len(b))}, b...))
	}
	digest := sha256.Sum256(b)
	return PeerID(append([]byte{0x12, 32}, digest[:]...))
}
func (id PeerID) Bytes() []byte  { return []byte(id) }
func (id PeerID) String() string { return base58.Encode([]byte(id)) }
func (id PeerID) Base32() string {
	s, _ := mb.Encode(mb.Base32, append([]byte{1, 0x72}, []byte(id)...))
	return s
}
func (id PeerID) Base36() string {
	s, _ := mb.Encode(mb.Base36, append([]byte{1, 0x72}, []byte(id)...))
	return s
}

// PeerIDFromBytes has a fixed 44-byte bound imposed by this identity profile.
func PeerIDFromBytes(b []byte) (PeerID, error) {
	if len(b) > 44 {
		return "", ErrLimit
	}
	code, n, err := varint.Decode(b)
	if err != nil {
		return "", err
	}
	size, m, err := varint.Decode(b[n:])
	if err != nil {
		return "", err
	}
	data := b[n+m:]
	if size != uint64(len(data)) {
		return "", ErrFormat
	}
	switch code {
	case 0:
		if len(data) > 42 {
			return "", ErrFormat
		}
		if _, err := ParsePublicKey(data, 42); err != nil {
			return "", err
		}
	case 0x12:
		if len(data) != 32 {
			return "", ErrFormat
		}
	default:
		return "", ErrFormat
	}
	return PeerID(b), nil
}

// ParsePeerID accepts legacy base58btc and multibase CIDv1 libp2p-key strings.
// Text is bounded before base decoding (128 bytes suffices for this profile).
func ParsePeerID(s string) (PeerID, error) {
	if len(s) > 128 {
		return "", ErrLimit
	}
	if s == "" {
		return "", ErrFormat
	}
	if strings.HasPrefix(s, "Qm") || s[0] == '1' {
		b, err := base58.Decode(s)
		if err != nil {
			return "", err
		}
		return PeerIDFromBytes(b)
	}
	_, b, err := mb.Decode(s)
	if err != nil {
		return "", err
	}
	version, n, err := varint.Decode(b)
	if err != nil {
		return "", err
	}
	codec, m, err := varint.Decode(b[n:])
	if err != nil {
		return "", err
	}
	if version != 1 || codec != 0x72 {
		return "", ErrFormat
	}
	return PeerIDFromBytes(b[n+m:])
}
