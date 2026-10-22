package bridge

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/mr-tron/base58"
)

const (
	codecRaw   uint64 = 0x55
	codecDagPB uint64 = 0x70
	hashSHA256 uint64 = 0x12
)

type IPFSCID struct {
	Version, Codec, Hash uint64
	Digest               [32]byte
}

func ParseIPFSCID(s string) (IPFSCID, error) {
	var c IPFSCID
	if strings.HasPrefix(s, "Qm") {
		b, e := base58.Decode(s)
		if e != nil || len(b) != 34 || b[0] != 0x12 || b[1] != 32 {
			return c, errors.New("ipfs cid: invalid CIDv0")
		}
		c.Version = 0
		c.Codec = codecDagPB
		c.Hash = hashSHA256
		copy(c.Digest[:], b[2:])
		return c, nil
	}
	if !strings.HasPrefix(s, "b") {
		return c, errors.New("ipfs cid: expected CIDv0 or base32 CIDv1")
	}
	enc := base32.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567").WithPadding(base32.NoPadding)
	b, e := enc.DecodeString(strings.ToUpper(s[1:]))
	if e != nil {
		return c, fmt.Errorf("ipfs cid: %w", e)
	}
	v, n := binary.Uvarint(b)
	if n <= 0 || v != 1 {
		return c, errors.New("ipfs cid: invalid version")
	}
	b = b[n:]
	codec, n := binary.Uvarint(b)
	if n <= 0 {
		return c, errors.New("ipfs cid: invalid codec")
	}
	b = b[n:]
	h, n := binary.Uvarint(b)
	if n <= 0 || h != hashSHA256 {
		return c, errors.New("ipfs cid: only sha2-256 is supported")
	}
	b = b[n:]
	z, n := binary.Uvarint(b)
	if n <= 0 || z != 32 || len(b[n:]) != 32 {
		return c, errors.New("ipfs cid: invalid digest")
	}
	if codec != codecRaw && codec != codecDagPB {
		return c, errors.New("ipfs cid: unsupported codec")
	}
	c.Version = v
	c.Codec = codec
	c.Hash = h
	copy(c.Digest[:], b[n:])
	return c, nil
}
func (c IPFSCID) Verify(b []byte) error {
	if c.Hash != hashSHA256 {
		return errors.New("ipfs cid: unsupported hash")
	}
	d := sha256.Sum256(b)
	if d != c.Digest {
		return errors.New("ipfs cid: block digest mismatch")
	}
	return nil
}
func (c IPFSCID) String() string {
	mh := append([]byte{0x12, 32}, c.Digest[:]...)
	if c.Version == 0 {
		return base58.Encode(mh)
	}
	b := appendVarint(nil, 1)
	b = appendVarint(b, c.Codec)
	b = append(b, mh...)
	return "b" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}
func appendVarint(b []byte, v uint64) []byte {
	var x [10]byte
	n := binary.PutUvarint(x[:], v)
	return append(b, x[:n]...)
}
