// Package car reads and writes DASL CAR (CAR v1) streams, verifying every block against its
// CID as it arrives. A CAR whose blocks use BLAKE3 CIDs is a BDASL CAR; the layout is the
// same.
package car

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/varint"
)

var (
	ErrCARv2 = errors.New("car: this is a CARv2 file; only CAR v1 (DASL CAR) is supported. " +
		"Extract the inner CARv1 payload (for example `car extract` / `go-car`'s inner data) and retry")
	ErrHeader = errors.New("car: invalid header")
)

// Header is a CAR v1 header. Extra holds every key besides version and roots (for example
// MASL metadata or Nu's `io.ipfs.nu` bundle manifest), as generic DRISL values.
type Header struct {
	Roots []cid.CID
	Extra map[string]any
}

// Block is one CID-addressed object.
type Block struct {
	CID  cid.CID
	Data []byte
}

// Options bounds untrusted input. Zero fields take the defaults.
type Options struct {
	MaxHeaderSize int // default 1 MiB
	MaxBlockSize  int // default cid.MaxWholeObject: CAR blocks are verified whole
}

// Reader streams verified blocks from a CAR.
type Reader struct {
	r      *bufio.Reader
	opts   Options
	header Header
	seen   map[cid.CID]bool // block present; value: referenced
	refs   map[cid.CID]bool
	order  []cid.CID
}

// NewReader reads and validates the header.
func NewReader(r io.Reader, opts Options) (*Reader, error) {
	if opts.MaxHeaderSize == 0 {
		opts.MaxHeaderSize = 1 << 20
	}
	if opts.MaxBlockSize == 0 {
		opts.MaxBlockSize = cid.MaxWholeObject
	}
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 64<<10)
	}
	cr := &Reader{r: br, opts: opts, seen: map[cid.CID]bool{}, refs: map[cid.CID]bool{}}
	n, err := varint.Read(br)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHeader, err)
	}
	if n == 0 || n > uint64(opts.MaxHeaderSize) {
		return nil, fmt.Errorf("%w: header length %d", ErrHeader, n)
	}
	hb := make([]byte, n)
	if _, err := io.ReadFull(br, hb); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHeader, err)
	}
	v, err := drisl.Decode(hb)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHeader, err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: not a map", ErrHeader)
	}
	switch m["version"] {
	case int64(1):
	case int64(2):
		return nil, ErrCARv2
	default:
		return nil, fmt.Errorf("%w: version %v", ErrHeader, m["version"])
	}
	roots, ok := m["roots"].([]any)
	if !ok {
		return nil, fmt.Errorf("%w: roots must be an array", ErrHeader)
	}
	for _, x := range roots {
		c, ok := x.(cid.CID)
		if !ok {
			return nil, fmt.Errorf("%w: root is not a CID", ErrHeader)
		}
		cr.header.Roots = append(cr.header.Roots, c)
		cr.refs[c] = true
	}
	cr.header.Extra = map[string]any{}
	for k, x := range m {
		if k != "version" && k != "roots" {
			cr.header.Extra[k] = x
		}
	}
	return cr, nil
}

// Header returns the full header.
func (r *Reader) Header() Header { return r.header }

// Next returns the next block after verifying it, or io.EOF at a clean end. Duplicates are
// returned again with dup set. DRISL blocks are decoded strictly and their links recorded.
func (r *Reader) Next() (b Block, dup bool, err error) {
	n, err := varint.Read(r.r)
	if err == io.EOF {
		return Block{}, false, io.EOF
	}
	if err != nil {
		return Block{}, false, fmt.Errorf("car: section length: %w", err)
	}
	if n < cid.Size || n > uint64(cid.Size+r.opts.MaxBlockSize) {
		return Block{}, false, fmt.Errorf("car: section length %d out of bounds", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r.r, buf); err != nil {
		return Block{}, false, fmt.Errorf("car: truncated block: %w", io.ErrUnexpectedEOF)
	}
	c, err := cid.Decode(buf[:cid.Size])
	if err != nil {
		return Block{}, false, fmt.Errorf("car: block CID: %w", err)
	}
	data := buf[cid.Size:]
	if err := c.Verify(data); err != nil {
		return Block{}, false, fmt.Errorf("car: block %s: %w", c, err)
	}
	if _, dup = r.seen[c]; dup {
		return Block{c, data}, true, nil
	}
	if c.Codec() == cid.DRISL {
		v, err := drisl.DecOptions{ForeignLinks: true}.Decode(data)
		if err != nil {
			return Block{}, false, fmt.Errorf("car: block %s: %w", c, err)
		}
		drisl.Links(v, func(l cid.CID) { r.refs[l] = true })
	}
	r.seen[c] = true
	r.order = append(r.order, c)
	return Block{c, data}, false, nil
}

// Unreferenced lists blocks seen so far that are neither roots nor linked from any block seen
// so far, in arrival order. Call it after Next returns io.EOF.
func (r *Reader) Unreferenced() []cid.CID {
	var out []cid.CID
	for _, c := range r.order {
		if !r.refs[c] {
			out = append(out, c)
		}
	}
	return out
}

// Writer emits a DASL CAR.
type Writer struct {
	w   io.Writer
	buf []byte
}

// NewWriter writes the header. Extra keys may not override version or roots.
func NewWriter(w io.Writer, h Header) (*Writer, error) {
	m := map[string]any{}
	for k, v := range h.Extra {
		m[k] = v
	}
	roots := make([]any, len(h.Roots))
	for i, c := range h.Roots {
		roots[i] = c
	}
	m["version"] = int64(1)
	m["roots"] = roots
	hb, err := drisl.Marshal(m)
	if err != nil {
		return nil, err
	}
	cw := &Writer{w: w}
	if err := cw.section(hb); err != nil {
		return nil, err
	}
	return cw, nil
}

func (w *Writer) section(parts ...[]byte) error {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	w.buf = varint.Append(w.buf[:0], uint64(n))
	for _, p := range parts {
		w.buf = append(w.buf, p...)
	}
	_, err := w.w.Write(w.buf)
	return err
}

// Put writes one block. It does not re-hash; callers write blocks they hold verified.
func (w *Writer) Put(b Block) error {
	if !b.CID.Defined() {
		return errors.New("car: undefined CID")
	}
	return w.section(b.CID.Bytes(), b.Data)
}

// LinksFunc returns the child links of a block in the order a traversal should visit them.
type LinksFunc func(c cid.CID, data []byte) ([]cid.CID, error)

// DRISLLinks visits every tag-42 link of DRISL blocks in canonical encoded order; raw blocks
// have none.
func DRISLLinks(c cid.CID, data []byte) ([]cid.CID, error) {
	if c.Codec() != cid.DRISL {
		return nil, nil
	}
	v, err := drisl.DecOptions{ForeignLinks: true}.Decode(data)
	if err != nil {
		return nil, err
	}
	var out []cid.CID
	drisl.Links(v, func(l cid.CID) { out = append(out, l) })
	return out, nil
}

// PreOrder visits blocks depth-first from roots, each block once, children in the order
// links returns them. It is the shared ordering helper: Nu tree pre-order passes the tree's
// links function, atproto streamable order passes commit → MST with records interleaved in
// entry order. get may return (nil, nil) to skip a block it does not hold.
func PreOrder(roots []cid.CID, get func(cid.CID) ([]byte, error), links LinksFunc, visit func(Block) error) error {
	seen := map[cid.CID]bool{}
	var rec func(c cid.CID) error
	rec = func(c cid.CID) error {
		if seen[c] {
			return nil
		}
		seen[c] = true
		data, err := get(c)
		if err != nil || data == nil {
			return err
		}
		if err := visit(Block{c, data}); err != nil {
			return err
		}
		kids, err := links(c, data)
		if err != nil {
			return err
		}
		for _, k := range kids {
			if err := rec(k); err != nil {
				return err
			}
		}
		return nil
	}
	for _, r := range roots {
		if err := rec(r); err != nil {
			return err
		}
	}
	return nil
}
