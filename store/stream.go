package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
)

// watchers lets readers wait for objects that are being fetched: committed objects and
// writable partial blobs opened in this process are announced by multihash, and live
// partials are registered so a reader can follow their verified groups.
type watchers struct {
	mu   sync.Mutex
	wait map[string][]chan struct{}
	live map[string]*Partial
}

// watch registers interest in m; the channel closes at m's next announcement.
func (s *Store) watch(m cid.Multihash) chan struct{} {
	ch := make(chan struct{})
	w := &s.watchers
	w.mu.Lock()
	if w.wait == nil {
		w.wait = map[string][]chan struct{}{}
	}
	k := string(mhKey(m))
	w.wait[k] = append(w.wait[k], ch)
	w.mu.Unlock()
	return ch
}

func (s *Store) unwatch(m cid.Multihash, ch chan struct{}) {
	w := &s.watchers
	w.mu.Lock()
	defer w.mu.Unlock()
	k := string(mhKey(m))
	for i, c := range w.wait[k] {
		if c == ch {
			w.wait[k] = append(w.wait[k][:i], w.wait[k][i+1:]...)
			break
		}
	}
	if len(w.wait[k]) == 0 {
		delete(w.wait, k)
	}
}

// announce wakes the waiters of each multihash (after a commit made them held, or a
// writable partial of one was opened).
func (s *Store) announce(ms ...cid.Multihash) {
	w := &s.watchers
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.wait) == 0 {
		return
	}
	for _, m := range ms {
		k := string(mhKey(m))
		for _, ch := range w.wait[k] {
			close(ch)
		}
		delete(w.wait, k)
	}
}

func (s *Store) setLive(p *Partial, live bool) {
	w := &s.watchers
	w.mu.Lock()
	k := string(mhKey(p.MH))
	switch {
	case live:
		if w.live == nil {
			w.live = map[string]*Partial{}
		}
		w.live[k] = p
	case w.live[k] == p:
		delete(w.live, k)
	}
	w.mu.Unlock()
	if live {
		s.announce(p.MH)
	}
}

func (s *Store) livePartial(m cid.Multihash) *Partial {
	w := &s.watchers
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.live[string(mhKey(m))]
}

// ErrNotArriving is OpenStream's error when an object is not held and done says no
// more data is coming.
var ErrNotArriving = errors.New("store: object is not held and is not arriving")

// OpenStream opens raw blob m for one sequential read while it may still be arriving, as
// `nu get` writes files while it fetches them. A complete blob reads as OpenBlob's. While
// a fetch in this process fills a partial of it, reads return its verified bytes in order
// and wait for the next group. A blob neither held nor arriving yet is waited for. When
// done is closed (the fetch returned), a blob that is not complete, or a partial that
// closes incomplete, ends the read with ErrNotArriving. ctx bounds every wait.
func (s *Store) OpenStream(ctx context.Context, m cid.Multihash, done <-chan struct{}) (io.ReadCloser, error) {
	r := &streamReader{s: s, m: m, ctx: ctx, done: done}
	if err := r.attach(); err != nil {
		return nil, err
	}
	return r, nil
}

type streamReader struct {
	s    *Store
	m    cid.Multihash
	ctx  context.Context
	done <-chan struct{}
	off  int64
	// Exactly one source is set once attached: a complete blob's reader, or a live
	// partial with this reader's own descriptor of its data file.
	whole io.Reader
	cl    io.Closer
	p     *Partial
	f     *os.File
}

// attach waits until m is complete or has a live partial, and opens it.
func (r *streamReader) attach() error {
	for {
		ch := r.s.watch(r.m)
		ok, err := r.tryAttach()
		if ok || err != nil {
			r.s.unwatch(r.m, ch)
			return err
		}
		select {
		case <-ch:
			continue
		case <-r.done:
			r.s.unwatch(r.m, ch)
			// The fetch returned: its last commit happened before, so one more look
			// decides.
			if ok, err := r.tryAttach(); ok || err != nil {
				return err
			}
			return fmt.Errorf("%w: %s", ErrNotArriving, r.m)
		case <-r.ctx.Done():
			r.s.unwatch(r.m, ch)
			return r.ctx.Err()
		}
	}
}

func (r *streamReader) tryAttach() (bool, error) {
	b, err := r.s.OpenBlob(r.m)
	if err == nil {
		if r.off > int64(b.Size) {
			b.Close()
			return false, fmt.Errorf("store: blob %s is shorter than its partial", r.m)
		}
		r.whole, r.cl = io.NewSectionReader(b, r.off, int64(b.Size)-r.off), b
		return true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	p := r.s.livePartial(r.m)
	if p == nil {
		return false, nil
	}
	f, err := os.Open(r.s.partialPath(r.m))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // committed or discarded meanwhile: look again
	} else if err != nil {
		return false, err
	}
	// The file must be the one p writes (not a newer partial's), and p still open.
	p.mu.Lock()
	same := !p.closed && sameFile(f, p.f)
	p.mu.Unlock()
	if !same {
		f.Close()
		return false, nil
	}
	r.p, r.f = p, f
	return true, nil
}

func sameFile(a, b *os.File) bool {
	ai, err := a.Stat()
	if err != nil {
		return false
	}
	bi, err := b.Stat()
	return err == nil && os.SameFile(ai, bi)
}

func (r *streamReader) detach() {
	if r.cl != nil {
		r.cl.Close()
	}
	if r.f != nil {
		r.f.Close()
	}
	r.whole, r.cl, r.p, r.f = nil, nil, nil, nil
}

func (r *streamReader) Read(b []byte) (int, error) {
	for {
		if r.whole != nil {
			n, err := r.whole.Read(b)
			r.off += int64(n)
			return n, err
		}
		if r.p == nil {
			if err := r.attach(); err != nil {
				return 0, err
			}
			continue
		}
		n, ok, err := r.readPartial(b)
		if ok || err != nil {
			return n, err
		}
		// The partial closed without this group: wait for a newer one or the blob.
		r.detach()
	}
}

// readPartial reads the held groups from r.off on, waiting for the first one. ok is false
// when the partial was closed without it, or the blob was announced meanwhile (committed
// through another handle, or a newer partial opened) or the fetch returned: the caller
// re-attaches.
func (r *streamReader) readPartial(b []byte) (int, bool, error) {
	p := r.p
	size := int64(p.Size)
	if r.off >= size {
		return 0, true, io.EOF
	}
	g := uint64(r.off) / blob.GroupSize
	p.mu.Lock()
	if !p.has(g) && !p.closed {
		woke := false
		ch, stopc := r.s.watch(r.m), make(chan struct{})
		go func() {
			select {
			case <-ch:
			case <-r.done:
			case <-r.ctx.Done():
			case <-stopc:
				return
			}
			p.mu.Lock()
			woke = true
			p.cond.Broadcast()
			p.mu.Unlock()
		}()
		for !p.has(g) && !p.closed && !woke {
			p.cond.Wait()
		}
		close(stopc)
		r.s.unwatch(r.m, ch)
	}
	if !p.has(g) {
		p.mu.Unlock()
		if err := r.ctx.Err(); err != nil {
			return 0, false, err
		}
		return 0, false, nil
	}
	// Read through every consecutive held group that fits.
	end := min(int64(g+1)*blob.GroupSize, size)
	for n := blob.Groups(p.Size); end < size && end-r.off < int64(len(b)); {
		next := uint64(end) / blob.GroupSize
		if next >= n || !p.has(next) {
			break
		}
		end = min(int64(next+1)*blob.GroupSize, size)
	}
	p.mu.Unlock()
	want := min(int64(len(b)), end-r.off)
	n, err := r.f.ReadAt(b[:want], r.off)
	r.off += int64(n)
	if int64(n) == want {
		err = nil
	} else if err == io.EOF || err == nil {
		err = fmt.Errorf("store: partial %s is shorter than its verified groups: %w", r.m, io.ErrUnexpectedEOF)
	}
	return n, true, err
}

func (r *streamReader) Close() error {
	r.detach()
	return nil
}
