package proto

import "io"

// readAheadAt amortizes the tiny positional reads in Bao's depth-first traversal.
// It is request-local (no locks or cross-request stale cache) and bounded,
// including for arbitrarily large blobs. Parent paths may seek backwards.
type readAheadAt struct {
	src io.ReaderAt
	buf []byte
	off int64
	n   int
}

func newReadAheadAt(src io.ReaderAt, size int) *readAheadAt {
	return &readAheadAt{src: src, buf: make([]byte, size)}
}

func (r *readAheadAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 || len(p) > len(r.buf) {
		return r.src.ReadAt(p, off)
	}
	if off < r.off || off-r.off > int64(r.n) || int64(len(p)) > int64(r.n)-(off-r.off) {
		n, err := r.src.ReadAt(r.buf, off)
		r.off, r.n = off, n
		if n < len(p) {
			return copy(p, r.buf[:n]), err
		}
	}
	return copy(p, r.buf[off-r.off:int(off-r.off)+len(p)]), nil
}
