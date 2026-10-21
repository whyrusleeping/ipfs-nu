// Package cid implements DASL and BDASL CIDs: CIDv1 with codec raw (0x55) or DRISL (0x71)
// and a 32-byte sha2-256 (0x12) or blake3 (0x1e) multihash. The binary form is always 36
// bytes and the text form is "b" + lowercase unpadded base32. Everything else is rejected.
//
// CIDs are hand-rolled rather than built on go-cid: every accepted prefix is four fixed
// bytes, so strict parsing is a byte comparison (see docs/dasl.md, D21).
package cid

import (
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"hash"

	"lukechampine.com/blake3"
)

// Codec is a CID content codec.
type Codec byte

// Hash is a multihash function code.
type Hash byte

const (
	Raw   Codec = 0x55
	DRISL Codec = 0x71

	SHA256 Hash = 0x12
	BLAKE3 Hash = 0x1e
)

const (
	// Size is the length of a binary CID.
	Size = 36
	// DigestSize is the only accepted digest length.
	DigestSize = 32
	// MultihashSize is the length of a binary multihash.
	MultihashSize = 2 + DigestSize
	// MaxWholeObject is the fixed protocol limit for objects that can only be verified as a
	// whole: every SHA-256 object and BLAKE3 DRISL (D21).
	MaxWholeObject = 8 << 20
)

var (
	ErrInvalid  = errors.New("cid: not a DASL/BDASL CID")
	ErrMismatch = errors.New("cid: content does not match hash")
	ErrTooLarge = fmt.Errorf("cid: object exceeds the %d-byte whole-object limit; large content must be a BLAKE3 raw blob", MaxWholeObject)
)

// Encoding is lowercase RFC 4648 base32 without padding.
var Encoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func (c Codec) String() string {
	switch c {
	case Raw:
		return "raw"
	case DRISL:
		return "drisl"
	}
	return fmt.Sprintf("codec(0x%x)", byte(c))
}

func (h Hash) String() string {
	switch h {
	case SHA256:
		return "sha2-256"
	case BLAKE3:
		return "blake3"
	}
	return fmt.Sprintf("hash(0x%x)", byte(h))
}

// Valid reports whether h is an accepted hash function.
func (h Hash) Valid() bool { return h == SHA256 || h == BLAKE3 }

// Valid reports whether c is an accepted codec.
func (c Codec) Valid() bool { return c == Raw || c == DRISL }

// New returns a hasher for h. It panics on an unsupported hash.
func (h Hash) New() hash.Hash {
	switch h {
	case SHA256:
		return sha256.New()
	case BLAKE3:
		return blake3.New(DigestSize, nil)
	}
	panic("cid: unsupported hash " + h.String())
}

// Multihash is a 32-byte sha2-256 or blake3 multihash. The zero value is undefined.
// Objects are keyed by multihash: the codec does not change the bytes.
type Multihash struct {
	Hash   Hash
	Digest [DigestSize]byte
}

// Sum hashes data. It does not apply the whole-object limit; see SumCID.
func Sum(h Hash, data []byte) Multihash {
	m := Multihash{Hash: h}
	switch h {
	case SHA256:
		m.Digest = sha256.Sum256(data)
	case BLAKE3:
		m.Digest = blake3.Sum256(data)
	default:
		panic("cid: unsupported hash " + h.String())
	}
	return m
}

// FromHasher finishes hasher hs (created by h.New) into a multihash.
func FromHasher(h Hash, hs hash.Hash) Multihash {
	m := Multihash{Hash: h}
	copy(m.Digest[:], hs.Sum(nil))
	return m
}

// Defined reports whether m is not the zero value.
func (m Multihash) Defined() bool { return m.Hash != 0 }

// Bytes returns the 34-byte binary multihash.
func (m Multihash) Bytes() []byte { return m.Append(make([]byte, 0, MultihashSize)) }

// Append appends the binary multihash to b.
func (m Multihash) Append(b []byte) []byte {
	b = append(b, byte(m.Hash), DigestSize)
	return append(b, m.Digest[:]...)
}

// String returns "b" + base32 of the binary multihash, the form used for keys and file names.
func (m Multihash) String() string { return "b" + Encoding.EncodeToString(m.Bytes()) }

// DecodeMultihash parses exactly one 34-byte multihash.
func DecodeMultihash(b []byte) (Multihash, error) {
	if len(b) != MultihashSize || !Hash(b[0]).Valid() || b[1] != DigestSize {
		return Multihash{}, ErrInvalid
	}
	m := Multihash{Hash: Hash(b[0])}
	copy(m.Digest[:], b[2:])
	return m, nil
}

// ParseMultihash parses the text form produced by Multihash.String.
func ParseMultihash(s string) (Multihash, error) {
	b, err := decodeText(s, MultihashSize)
	if err != nil {
		return Multihash{}, err
	}
	return DecodeMultihash(b)
}

// CID is a DASL/BDASL CID. It is comparable; the zero value is undefined.
type CID struct {
	codec Codec
	mh    Multihash
}

// New returns the CID for codec c and multihash m. It panics on unsupported values.
func New(c Codec, m Multihash) CID {
	if !c.Valid() || !m.Hash.Valid() {
		panic("cid: unsupported codec or hash")
	}
	return CID{c, m}
}

// SumCID hashes data into a CID, enforcing the whole-object limit for whole-object-verified
// kinds (see WholeObject).
func SumCID(c Codec, h Hash, data []byte) (CID, error) {
	if !c.Valid() || !h.Valid() {
		return CID{}, ErrInvalid
	}
	if WholeObject(c, h) && len(data) > MaxWholeObject {
		return CID{}, ErrTooLarge
	}
	return CID{c, Sum(h, data)}, nil
}

// WholeObject reports whether objects of this kind can only be verified as a whole and are
// therefore limited to MaxWholeObject: everything except BLAKE3 raw blobs.
func WholeObject(c Codec, h Hash) bool { return !(c == Raw && h == BLAKE3) }

// Verify checks that data hashes to c, applying the whole-object limit where it applies.
func (c CID) Verify(data []byte) error {
	if !c.Defined() {
		return ErrInvalid
	}
	if WholeObject(c.codec, c.mh.Hash) && len(data) > MaxWholeObject {
		return ErrTooLarge
	}
	if Sum(c.mh.Hash, data) != c.mh {
		return ErrMismatch
	}
	return nil
}

// Defined reports whether c is not the zero value.
func (c CID) Defined() bool { return c.codec != 0 }

// Codec returns the content codec.
func (c CID) Codec() Codec { return c.codec }

// Multihash returns the multihash.
func (c CID) Multihash() Multihash { return c.mh }

// Hash returns the hash function.
func (c CID) Hash() Hash { return c.mh.Hash }

// Bytes returns the 36-byte binary CID.
func (c CID) Bytes() []byte { return c.Append(make([]byte, 0, Size)) }

// Append appends the binary CID to b.
func (c CID) Append(b []byte) []byte {
	b = append(b, 1, byte(c.codec))
	return c.mh.Append(b)
}

// String returns the text form ("" for the zero value).
func (c CID) String() string {
	if !c.Defined() {
		return ""
	}
	return "b" + Encoding.EncodeToString(c.Bytes())
}

// Decode parses exactly one binary CID.
func Decode(b []byte) (CID, error) {
	if len(b) != Size || b[0] != 1 || !Codec(b[1]).Valid() {
		return CID{}, ErrInvalid
	}
	m, err := DecodeMultihash(b[2:])
	if err != nil {
		return CID{}, err
	}
	return CID{Codec(b[1]), m}, nil
}

// Parse parses the text form. Only the canonical lowercase encoding is accepted.
func Parse(s string) (CID, error) {
	b, err := decodeText(s, Size)
	if err != nil {
		return CID{}, err
	}
	return Decode(b)
}

// MustParse is Parse for constants in tests and code; it panics on error.
func MustParse(s string) CID {
	c, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return c
}

func decodeText(s string, n int) ([]byte, error) {
	if len(s) != 1+Encoding.EncodedLen(n) || s[0] != 'b' {
		return nil, ErrInvalid
	}
	b, err := Encoding.DecodeString(s[1:])
	if err != nil || len(b) != n || Encoding.EncodeToString(b) != s[1:] {
		return nil, ErrInvalid // also rejects non-zero trailing bits
	}
	return b, nil
}

// MarshalText implements encoding.TextMarshaler.
func (c CID) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (c *CID) UnmarshalText(b []byte) error {
	v, err := Parse(string(b))
	if err != nil {
		return err
	}
	*c = v
	return nil
}
