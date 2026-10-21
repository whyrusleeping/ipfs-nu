// Package varint implements the multiformats unsigned varint: minimal encodings only, at
// most 9 bytes (63 bits). It is shared by CIDs, multihashes, multiaddrs and stream framing.
package varint

import (
	"encoding/binary"
	"errors"
)

// MaxLen is the longest valid encoding.
const MaxLen = 9

var (
	ErrTruncated  = errors.New("varint: truncated")
	ErrOverflow   = errors.New("varint: longer than 9 bytes")
	ErrNotMinimal = errors.New("varint: not minimally encoded")
)

// Append appends the encoding of v to b. v must be below 2^63.
func Append(b []byte, v uint64) []byte {
	return binary.AppendUvarint(b, v)
}

// Len returns the encoded length of v.
func Len(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// Decode reads a varint from the start of b and returns the value and the bytes consumed.
func Decode(b []byte) (uint64, int, error) {
	var v uint64
	for i := 0; i < len(b); i++ {
		if i == MaxLen {
			return 0, 0, ErrOverflow
		}
		c := b[i]
		v |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			if c == 0 && i > 0 {
				return 0, 0, ErrNotMinimal
			}
			return v, i + 1, nil
		}
	}
	if len(b) >= MaxLen {
		return 0, 0, ErrOverflow
	}
	return 0, 0, ErrTruncated
}

// ByteReader is the subset of io.ByteReader Read needs.
type ByteReader interface{ ReadByte() (byte, error) }

// Read reads one varint from r. A clean EOF before the first byte is returned as-is.
func Read(r ByteReader) (uint64, error) {
	var buf [MaxLen]byte
	for i := 0; i < MaxLen; i++ {
		c, err := r.ReadByte()
		if err != nil {
			if i > 0 {
				return 0, ErrTruncated
			}
			return 0, err
		}
		buf[i] = c
		if c < 0x80 {
			v, _, err := Decode(buf[:i+1])
			return v, err
		}
	}
	return 0, ErrOverflow
}
