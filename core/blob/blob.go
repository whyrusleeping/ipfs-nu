// Package blob implements Nu's raw blobs: whole-file BLAKE3 hashes with Bao outboards at
// 16 KiB chunk groups, verified range slices, and whole-object checks for small and SHA-256
// blobs (D2, D21).
//
// Outboard layout (standard Bao outboard, group log 4): an 8-byte little-endian blob size,
// then the parent nodes (64 bytes: left and right chaining values) in pre-order. A slice for
// a range of chunk groups is a standard Bao slice: the 8-byte size, then in pre-order every
// parent on the path to the range and the range's chunk groups.
package blob

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"

	"lukechampine.com/blake3/bao"
	"lukechampine.com/blake3/guts"

	"github.com/whyrusleeping/nu/core/cid"
)

const (
	// GroupLog is log2 of the chunks per chunk group.
	GroupLog = 4
	// GroupSize is the verification granularity: 16 KiB.
	GroupSize = guts.ChunkSize << GroupLog
	// HeaderSize is the size prefix of outboards and slices.
	HeaderSize = 8
)

// ErrCorrupt reports data that does not match its hash.
var ErrCorrupt = errors.New("blob: data does not match hash")

// CorruptError locates a verification failure at a chunk group.
type CorruptError struct{ Group uint64 }

func (e *CorruptError) Error() string {
	return fmt.Sprintf("blob: chunk group %d does not verify", e.Group)
}
func (e *CorruptError) Unwrap() error { return ErrCorrupt }

// HasOutboard reports whether a BLAKE3 blob of this size keeps an outboard. Blobs of at most
// one chunk group are verified whole by hash.
func HasOutboard(size uint64) bool { return size > GroupSize }

// Groups is the number of chunk groups in a blob (an empty blob has one empty group).
func Groups(size uint64) uint64 { return groupsIn(size, GroupSize) }

func groupsIn(n, gs uint64) uint64 {
	if n == 0 {
		return 1
	}
	return (n + gs - 1) / gs
}

// OutboardSize is the byte length of the outboard for a blob of this size.
func OutboardSize(size uint64) uint64 { return HeaderSize + 64*(Groups(size)-1) }

// Range is a half-open range of chunk groups [Start, End).
type Range struct{ Start, End uint64 }

// Bytes returns the byte offset and length of r within a blob of the given size.
func (r Range) Bytes(size uint64) (off, n uint64) {
	off = r.Start * GroupSize
	end := min(r.End*GroupSize, size)
	if off > end {
		return off, 0
	}
	return off, end - off
}

// Valid reports whether r is a non-empty range inside a blob of the given size.
func (r Range) Valid(size uint64) bool { return r.Start < r.End && r.End <= Groups(size) }

// ByteRange maps a byte range [start, end) to the chunk groups that contain it (BDASL range
// rule: round the start down and the end up to group boundaries). end is clamped to size.
func ByteRange(start, end, size uint64) Range {
	end = min(end, size)
	if start >= end {
		g := min(start/GroupSize, Groups(size)-1)
		return Range{g, g + 1}
	}
	return Range{start / GroupSize, (end + GroupSize - 1) / GroupSize}
}

// Sum hashes a whole blob as BLAKE3 (streaming). It returns the multihash and size.
func Sum(r io.Reader) (cid.Multihash, uint64, error) {
	h := cid.BLAKE3.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return cid.Multihash{}, 0, err
	}
	return cid.FromHasher(cid.BLAKE3, h), uint64(n), nil
}

// Outboard reads exactly size bytes of blob data and writes its outboard to dst (which must
// accept OutboardSize(size) bytes; writes are not sequential). It returns the BLAKE3
// multihash. Memory use is constant in size.
func Outboard(dst io.WriterAt, data io.Reader, size uint64) (cid.Multihash, error) {
	root, err := bao.Encode(dst, data, int64(size), GroupLog, true)
	if err != nil {
		return cid.Multihash{}, err
	}
	return cid.Multihash{Hash: cid.BLAKE3, Digest: root}, nil
}

// ExtractSlice writes the Bao slice for groups r of a blob, reading blob data and its
// outboard by offset. Only the parents on the path and the groups in r are read.
func ExtractSlice(dst io.Writer, data, outboard io.ReaderAt, size uint64, r Range) error {
	if !r.Valid(size) {
		return fmt.Errorf("blob: range %v outside a %d-byte blob", r, size)
	}
	off, n := r.Bytes(size)
	return extract(dst, data, outboard, size, off, n, GroupLog)
}

func extract(dst io.Writer, data, outboard io.ReaderAt, size, off, n uint64, glog uint) error {
	gs := uint64(guts.ChunkSize << glog)
	var hdr [HeaderSize]byte
	if _, err := outboard.ReadAt(hdr[:], 0); err != nil {
		return fmt.Errorf("blob: outboard header: %w", err)
	}
	if binary.LittleEndian.Uint64(hdr[:]) != size {
		return errors.New("blob: outboard size does not match blob")
	}
	if _, err := dst.Write(hdr[:]); err != nil {
		return err
	}
	start, end := sliceBounds(off, n, size)
	buf := make([]byte, gs)
	var rec func(pos, l, obOff uint64) error
	rec = func(pos, l, obOff uint64) error {
		if !(pos < end && start < pos+l) && !(l == 0 && pos == 0) {
			return nil
		}
		if l <= gs {
			if _, err := data.ReadAt(buf[:l], int64(pos)); err != nil && !(err == io.EOF && l == 0) {
				return fmt.Errorf("blob: read data: %w", err)
			}
			_, err := dst.Write(buf[:l])
			return err
		}
		if _, err := outboard.ReadAt(buf[:64], int64(obOff)); err != nil {
			return fmt.Errorf("blob: read outboard: %w", err)
		}
		if _, err := dst.Write(buf[:64]); err != nil {
			return err
		}
		mid := uint64(1) << (bits.Len64(l-1) - 1)
		if err := rec(pos, mid, obOff+64); err != nil {
			return err
		}
		return rec(pos+mid, l-mid, obOff+64+64*(groupsIn(mid, gs)-1))
	}
	return rec(0, size, HeaderSize)
}

// sliceBounds applies Bao's slice rules: an empty slice covers one byte, and a slice that
// starts at or beyond the end covers the final byte.
func sliceBounds(off, n, size uint64) (start, end uint64) {
	if n == 0 {
		n = 1
	}
	if size > 0 && off >= size {
		off = size - 1
	}
	return off, min(off+n, size)
}

// Sink receives verified slice contents. Any field may be nil.
type Sink struct {
	// Data receives each verified chunk group at its offset in the blob.
	Data io.WriterAt
	// Outboard receives the header and every verified parent node at its outboard offset,
	// so a receiver accumulates the outboard needed to serve the groups it holds.
	Outboard io.WriterAt
	// Group is called after each group is verified and written.
	Group func(group uint64)
}

// DecodeSlice reads a Bao slice for groups r of the blob with the given BLAKE3 multihash and
// size from src, verifying each group before passing it to sink. On a mismatch it returns a
// *CorruptError naming the first group under the failing node; earlier groups were already
// verified and delivered. It reads exactly the slice and nothing after it.
func DecodeSlice(src io.Reader, mh cid.Multihash, size uint64, r Range, sink Sink) error {
	if mh.Hash != cid.BLAKE3 {
		return errors.New("blob: Bao slices need a BLAKE3 hash")
	}
	if !r.Valid(size) {
		return fmt.Errorf("blob: range %v outside a %d-byte blob", r, size)
	}
	off, n := r.Bytes(size)
	return decode(src, mh.Digest, size, off, n, GroupLog, sink)
}

func decode(src io.Reader, root [32]byte, size, off, n uint64, glog uint, sink Sink) error {
	gs := uint64(guts.ChunkSize << glog)
	buf := make([]byte, gs)
	if _, err := io.ReadFull(src, buf[:HeaderSize]); err != nil {
		return fmt.Errorf("blob: slice header: %w", err)
	}
	if binary.LittleEndian.Uint64(buf[:HeaderSize]) != size {
		return &CorruptError{0}
	}
	if sink.Outboard != nil {
		if _, err := sink.Outboard.WriteAt(buf[:HeaderSize], 0); err != nil {
			return err
		}
	}
	start, end := sliceBounds(off, n, size)
	var rec func(cv [8]uint32, pos, l, obOff uint64, flags uint32) error
	rec = func(cv [8]uint32, pos, l, obOff uint64, flags uint32) error {
		if !(pos < end && start < pos+l) && !(l == 0 && pos == 0) {
			return nil
		}
		if l <= gs {
			if _, err := io.ReadFull(src, buf[:l]); err != nil {
				return fmt.Errorf("blob: slice data: %w", err)
			}
			nd := compressGroup(buf[:l], pos/guts.ChunkSize)
			nd.Flags |= flags
			if guts.ChainingValue(nd) != cv {
				return &CorruptError{pos / GroupSize}
			}
			if sink.Data != nil && l > 0 {
				if _, err := sink.Data.WriteAt(buf[:l], int64(pos)); err != nil {
					return err
				}
			}
			if sink.Group != nil {
				sink.Group(pos / gs)
			}
			return nil
		}
		if _, err := io.ReadFull(src, buf[:64]); err != nil {
			return fmt.Errorf("blob: slice parent: %w", err)
		}
		left, right := bytesToCV(buf[:32]), bytesToCV(buf[32:64])
		if guts.ChainingValue(guts.ParentNode(left, right, &guts.IV, flags)) != cv {
			return &CorruptError{pos / GroupSize}
		}
		if sink.Outboard != nil {
			if _, err := sink.Outboard.WriteAt(buf[:64], int64(obOff)); err != nil {
				return err
			}
		}
		mid := uint64(1) << (bits.Len64(l-1) - 1)
		if err := rec(left, pos, mid, obOff+64, 0); err != nil {
			return err
		}
		return rec(right, pos+mid, l-mid, obOff+64+64*(groupsIn(mid, gs)-1), 0)
	}
	return rec(bytesToCV(root[:]), 0, size, HeaderSize, guts.FlagRoot)
}

// VerifyWhole checks a whole object against a multihash: the path for SHA-256 blobs and
// BLAKE3 blobs of at most one group. SHA-256 objects are limited to the whole-object size.
func VerifyWhole(data []byte, mh cid.Multihash) error {
	if mh.Hash == cid.SHA256 && len(data) > cid.MaxWholeObject {
		return cid.ErrTooLarge
	}
	if cid.Sum(mh.Hash, data) != mh {
		return ErrCorrupt
	}
	return nil
}

func bytesToCV(b []byte) (cv [8]uint32) {
	for i := range cv {
		cv[i] = binary.LittleEndian.Uint32(b[4*i:])
	}
	return cv
}

// compressGroup hashes one chunk group into its (non-root) node. It is adapted from
// lukechampine.com/blake3/bao (MIT License, Copyright (c) 2020 Luke Champine).
func compressGroup(p []byte, counter uint64) guts.Node {
	var stack [54 - guts.MaxSIMD][8]uint32
	var sc uint64
	pushSubtree := func(cv [8]uint32) {
		i := 0
		for sc&(1<<i) != 0 {
			cv = guts.ChainingValue(guts.ParentNode(stack[i], cv, &guts.IV, 0))
			i++
		}
		stack[i] = cv
		sc++
	}
	var buf [guts.MaxSIMD * guts.ChunkSize]byte
	var buflen int
	for len(p) > 0 {
		if buflen == len(buf) {
			pushSubtree(guts.ChainingValue(guts.CompressBuffer(&buf, buflen, &guts.IV, counter+(sc*guts.MaxSIMD), 0)))
			buflen = 0
		}
		n := copy(buf[buflen:], p)
		buflen += n
		p = p[n:]
	}
	n := guts.CompressBuffer(&buf, buflen, &guts.IV, counter+(sc*guts.MaxSIMD), 0)
	for i := bits.TrailingZeros64(sc); i < bits.Len64(sc); i++ {
		if sc&(1<<i) != 0 {
			n = guts.ParentNode(stack[i], guts.ChainingValue(n), &guts.IV, 0)
		}
	}
	return n
}
