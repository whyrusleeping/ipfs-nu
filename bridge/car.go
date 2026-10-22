package bridge

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	maxCARHeader  = 1 << 20
	maxCARSection = 16 << 20
)

// CARSource is a read-only, whole-block verified CARv1 source.
type carBlockRef struct {
	offset int64
	size   int64
}
type CARSource struct {
	Roots  []IPFSCID
	blocks map[IPFSCID]carBlockRef
	file   *os.File
	path   string
}

func NewCARSource(r io.Reader) (*CARSource, error) {
	br := bufio.NewReader(r)
	n, e := binary.ReadUvarint(br)
	if e != nil || n == 0 || n > maxCARHeader {
		return nil, errors.New("bridge: invalid CAR header length")
	}
	hb := make([]byte, int(n))
	if _, e = io.ReadFull(br, hb); e != nil {
		return nil, e
	}
	h, e := decodeCBOR(hb)
	if e != nil {
		return nil, fmt.Errorf("bridge: invalid CAR header: %w", e)
	}
	hm, ok := h.(map[string]any)
	if !ok || hm["version"] != uint64(1) {
		return nil, errors.New("bridge: only CARv1 is supported")
	}
	rootsAny, ok := hm["roots"].([]any)
	if !ok || len(rootsAny) == 0 {
		return nil, errors.New("bridge: CAR header has no roots")
	}
	c := &CARSource{blocks: map[IPFSCID]carBlockRef{}}
	for _, v := range rootsAny {
		b, ok := v.([]byte)
		if !ok {
			return nil, errors.New("bridge: invalid CAR root")
		}
		if len(b) > 0 && b[0] == 0 {
			b = b[1:]
		}
		id, e := ParseIPFSBytes(b)
		if e != nil {
			return nil, e
		}
		c.Roots = append(c.Roots, id)
	}
	file, e := os.CreateTemp("", "nu-ipfs-car-*")
	if e != nil {
		return nil, e
	}
	c.file, c.path = file, file.Name()
	complete := false
	defer func() {
		if !complete {
			_ = c.file.Close()
			_ = os.Remove(c.path)
		}
	}()
	for {
		ln, e := binary.ReadUvarint(br)
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, fmt.Errorf("bridge: CAR section length: %w", e)
		}
		if ln == 0 || ln > maxCARSection {
			return nil, errors.New("bridge: CAR section exceeds limit")
		}
		sec := make([]byte, int(ln))
		if _, e = io.ReadFull(br, sec); e != nil {
			return nil, io.ErrUnexpectedEOF
		}
		l, e := ipfsCIDLength(sec)
		if e != nil {
			return nil, e
		}
		id, e := ParseIPFSBytes(sec[:l])
		if e != nil {
			return nil, e
		}
		data := sec[l:]
		if e = id.Verify(data); e != nil {
			return nil, fmt.Errorf("bridge: CAR block %s: %w", id.String(), e)
		}
		if _, dup := c.blocks[id]; !dup {
			offset, e := c.file.Seek(0, io.SeekEnd)
			if e != nil {
				return nil, e
			}
			if _, e = c.file.Write(data); e != nil {
				return nil, e
			}
			c.blocks[id] = carBlockRef{offset: offset, size: int64(len(data))}
		}
	}
	if err := c.file.Sync(); err != nil {
		return nil, err
	}
	complete = true
	return c, nil
}
func (c *CARSource) Blocks() int { return len(c.blocks) }
func (c *CARSource) GetBlock(ctx context.Context, id IPFSCID) ([]byte, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	ref, ok := c.blocks[id]
	if !ok {
		return nil, fmt.Errorf("bridge: CAR does not contain %s", id.String())
	}
	b := make([]byte, ref.size)
	if _, e := c.file.ReadAt(b, ref.offset); e != nil {
		return nil, e
	}
	if e := id.Verify(b); e != nil {
		return nil, e
	}
	return b, nil
}

// Close deletes the temporary disk-backed index; the original CAR is never modified.
func (c *CARSource) Close() error {
	if c.file == nil {
		return nil
	}
	err := c.file.Close()
	c.file = nil
	return errors.Join(err, os.Remove(c.path))
}
func ipfsCIDLength(b []byte) (int, error) {
	if len(b) >= 34 && b[0] == 0x12 && b[1] == 32 {
		return 34, nil
	}
	off := 0
	for k := 0; k < 4; k++ {
		v, n := binary.Uvarint(b[off:])
		if n <= 0 {
			return 0, errors.New("bridge: malformed CID in CAR")
		}
		off += n
		if k == 3 {
			if v > uint64(len(b)-off) {
				return 0, errors.New("bridge: truncated CID digest")
			}
			off += int(v)
		}
	}
	if off > len(b) {
		return 0, errors.New("bridge: truncated CID")
	}
	return off, nil
}
func decodeCBOR(b []byte) (any, error) {
	v, n, e := cborItem(b, 0)
	if e != nil {
		return nil, e
	}
	if n != len(b) {
		return nil, errors.New("trailing CBOR bytes")
	}
	return v, nil
}
func cborItem(b []byte, depth int) (any, int, error) {
	if depth > 32 || len(b) == 0 {
		return nil, 0, errors.New("CBOR depth/length limit")
	}
	head := b[0]
	major, ai := head>>5, head&31
	off := 1
	arg, e := cborArg(b, &off, ai)
	if e != nil {
		return nil, 0, e
	}
	switch major {
	case 0:
		return arg, off, nil
	case 2, 3:
		if arg > uint64(len(b)-off) {
			return nil, 0, io.ErrUnexpectedEOF
		}
		if major == 2 {
			return append([]byte(nil), b[off:off+int(arg)]...), off + int(arg), nil
		}
		return string(b[off : off+int(arg)]), off + int(arg), nil
	case 4:
		if arg > 1<<16 {
			return nil, 0, errors.New("CBOR array too large")
		}
		out := make([]any, 0, int(arg))
		for j := uint64(0); j < arg; j++ {
			x, n, e := cborItem(b[off:], depth+1)
			if e != nil {
				return nil, 0, e
			}
			off += n
			out = append(out, x)
		}
		return out, off, nil
	case 5:
		if arg > 1<<16 {
			return nil, 0, errors.New("CBOR map too large")
		}
		out := map[string]any{}
		for j := uint64(0); j < arg; j++ {
			kv, n, e := cborItem(b[off:], depth+1)
			if e != nil {
				return nil, 0, e
			}
			off += n
			k, ok := kv.(string)
			if !ok {
				return nil, 0, errors.New("CAR header map key is not text")
			}
			if _, dup := out[k]; dup {
				return nil, 0, errors.New("duplicate CBOR map key")
			}
			v, n, e := cborItem(b[off:], depth+1)
			if e != nil {
				return nil, 0, e
			}
			off += n
			out[k] = v
		}
		return out, off, nil
	case 6:
		if arg != 42 {
			return nil, 0, errors.New("unexpected CBOR tag")
		}
		v, n, e := cborItem(b[off:], depth+1)
		if e != nil {
			return nil, 0, e
		}
		raw, ok := v.([]byte)
		if !ok {
			return nil, 0, errors.New("CID tag is not bytes")
		}
		return raw, off + n, nil
	case 7:
		if ai == 20 {
			return false, off, nil
		}
		if ai == 21 {
			return true, off, nil
		}
		if ai == 22 {
			return nil, off, nil
		}
		return nil, 0, errors.New("unsupported CBOR simple value")
	default:
		return nil, 0, errors.New("invalid CBOR major type")
	}
}
func cborArg(b []byte, off *int, ai byte) (uint64, error) {
	switch {
	case ai < 24:
		return uint64(ai), nil
	case ai == 24:
		if len(b) < *off+1 {
			return 0, io.ErrUnexpectedEOF
		}
		v := uint64(b[*off])
		*off += 1
		return v, nil
	case ai == 25:
		if len(b) < *off+2 {
			return 0, io.ErrUnexpectedEOF
		}
		v := uint64(binary.BigEndian.Uint16(b[*off:]))
		*off += 2
		return v, nil
	case ai == 26:
		if len(b) < *off+4 {
			return 0, io.ErrUnexpectedEOF
		}
		v := uint64(binary.BigEndian.Uint32(b[*off:]))
		*off += 4
		return v, nil
	case ai == 27:
		if len(b) < *off+8 {
			return 0, io.ErrUnexpectedEOF
		}
		v := binary.BigEndian.Uint64(b[*off:])
		*off += 8
		return v, nil
	default:
		return 0, errors.New("indefinite CBOR values unsupported")
	}
}
