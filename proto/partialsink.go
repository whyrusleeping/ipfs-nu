package proto

import (
	"errors"
	"io"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/store"
)

// readPartialSlice batches already-verified writes. Group publication is delayed
// until BOTH files have accepted the corresponding bytes, so a concurrent Flush
// never persists a bitmap that refers to this request's userspace buffers.
// Even on a corrupt/truncated response, flush the verified prefix for resume.
func readPartialSlice(src io.Reader, p *store.Partial, rg blob.Range) error {
	dst := p.Sink()
	batch := partialBatch{
		dst:     dst,
		data:    pendingAt{dst: dst.Data, buf: make([]byte, 64<<10)},
		parents: pendingAt{dst: dst.Outboard, buf: make([]byte, 4<<10)},
	}
	err := readSlice(src, p.MH, p.Size, rg, blob.Sink{
		Data:     &batch,
		Outboard: &batch.parents,
		Group:    func(g uint64) { batch.groups = append(batch.groups, g) },
	})
	return errors.Join(err, batch.flush())
}

type partialBatch struct {
	dst           blob.Sink
	data, parents pendingAt
	groups        []uint64 // at most four 16 KiB groups
}

func (b *partialBatch) WriteAt(p []byte, off int64) (int, error) {
	if !b.data.fits(p, off) {
		if err := b.flush(); err != nil {
			return 0, err
		}
	}
	return b.data.WriteAt(p, off)
}

func (b *partialBatch) flush() error {
	if err := b.parents.flush(); err != nil {
		return err
	}
	if err := b.data.flush(); err != nil {
		return err
	}
	for _, g := range b.groups {
		b.dst.Group(g)
	}
	b.groups = b.groups[:0]
	return nil
}

// pendingAt coalesces contiguous positional writes with a fixed buffer. It never
// reads or fills gaps (outboard offsets need not be sequential).
type pendingAt struct {
	dst io.WriterAt
	buf []byte
	off int64
	n   int
}

func (w *pendingAt) fits(p []byte, off int64) bool {
	return (w.n == 0 || off == w.off+int64(w.n)) && len(p) <= len(w.buf)-w.n
}
func (w *pendingAt) WriteAt(p []byte, off int64) (int, error) {
	if !w.fits(p, off) {
		if err := w.flush(); err != nil {
			return 0, err
		}
	}
	if len(p) > len(w.buf) {
		return w.dst.WriteAt(p, off)
	}
	if w.n == 0 {
		w.off = off
	}
	w.n += copy(w.buf[w.n:], p)
	return len(p), nil
}
func (w *pendingAt) flush() error {
	if w.n == 0 {
		return nil
	}
	n, err := w.dst.WriteAt(w.buf[:w.n], w.off)
	if err != nil {
		return err
	}
	if n != w.n {
		return io.ErrShortWrite
	}
	w.n = 0
	return nil
}
