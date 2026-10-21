package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
)

// ErrComplete is returned by Partial when the blob is already held complete.
var ErrComplete = errors.New("store: blob already complete")

// Partial is a BLAKE3 blob being fetched by verified ranges. Verified chunk groups and the
// Bao parents on their paths are written to sparse files; the set of held groups is
// persisted by Flush, after the data is synced, so it never claims unsynced data. It
// survives restarts: calling Partial again resumes. Use one handle per blob.
type Partial struct {
	s      *Store
	MH     cid.Multihash
	Size   uint64
	f, ob  *os.File
	mu     sync.Mutex
	groups []byte // bitmap of verified groups
	dirty  bool
	held   int64 // verified bytes as last persisted
	// reserved is the admitted but unwritten bytes (A6): the missing bytes at open, less
	// what each Flush persisted; released on close.
	reserved int64
	readOnly bool
}

// Partial opens (or creates) the partial blob for a BLAKE3 multihash of the given size.
// Blobs that fit one chunk group are fetched whole with PutBlob instead. The size is a
// claim (A2): the missing bytes (plus the outboard on first open) are admitted against the
// budgets before anything is written, and sizes above MaxBlobSize are refused.
func (s *Store) Partial(m cid.Multihash, size uint64) (*Partial, error) {
	if m.Hash != cid.BLAKE3 || !blob.HasOutboard(size) {
		return nil, errors.New("store: partial blobs are BLAKE3 blobs larger than one chunk group; fetch others whole")
	}
	if size > MaxBlobSize {
		return nil, fmt.Errorf("store: declared blob size %d exceeds the %d-byte limit", size, MaxBlobSize)
	}
	if info, ok, err := s.Has(cid.New(cid.Raw, m)); err != nil {
		return nil, err
	} else if ok && info.Complete {
		return nil, ErrComplete
	}
	p := &Partial{s: s, MH: m, Size: size}
	var dbSize int64
	err := s.db.QueryRow(`SELECT size, groups, held FROM partials WHERE mh = ?`, mhKey(m)).Scan(&dbSize, &p.groups, &p.held)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := s.admit(int64(size + blob.OutboardSize(size))); err != nil {
			return nil, err
		}
		p.groups = make([]byte, (blob.Groups(size)+7)/8)
		s.gcMu.RLock()
		_, err = s.db.Exec(`INSERT INTO partials (mh, size, groups, held, updated) VALUES (?, ?, ?, 0, ?)`,
			mhKey(m), int64(size), p.groups, s.nowUnix())
		s.gcMu.RUnlock()
		if err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case uint64(dbSize) != size:
		return nil, fmt.Errorf("store: partial blob %s has size %d, not %d", m, dbSize, size)
	default:
		if err := s.admit(int64(size) - p.held); err != nil {
			return nil, err
		}
	}
	p.reserved = int64(size) - p.held
	s.reserve(p.reserved)
	path := s.partialPath(m)
	if p.f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err != nil {
		p.release()
		return nil, err
	}
	if p.ob, err = os.OpenFile(path+".obao", os.O_CREATE|os.O_RDWR, 0o600); err != nil {
		p.f.Close()
		p.release()
		return nil, err
	}
	return p, nil
}

// release returns the remaining reservation.
func (p *Partial) release() {
	p.mu.Lock()
	n := p.reserved
	p.reserved = 0
	p.mu.Unlock()
	p.s.reserve(-n)
}

// OpenPartial opens an existing partial blob read-only, for serving the groups it held when
// it was last flushed. It never creates one: ErrNotFound if there is none, ErrComplete if the
// blob has been completed.
func (s *Store) OpenPartial(m cid.Multihash) (*Partial, error) {
	p := &Partial{s: s, MH: m, readOnly: true}
	var size int64
	err := s.db.QueryRow(`SELECT size, groups FROM partials WHERE mh = ?`, mhKey(m)).Scan(&size, &p.groups)
	if errors.Is(err, sql.ErrNoRows) {
		if info, ok, err := s.Has(cid.New(cid.Raw, m)); err == nil && ok && info.Complete {
			return nil, ErrComplete
		}
		return nil, fmt.Errorf("%w: partial blob %s", ErrNotFound, m)
	}
	if err != nil {
		return nil, err
	}
	p.Size = uint64(size)
	path := s.partialPath(m)
	if p.f, err = os.Open(path); err != nil {
		return nil, err
	}
	if p.ob, err = os.Open(path + ".obao"); err != nil {
		p.f.Close()
		return nil, err
	}
	return p, nil
}

// DiscardPartial deletes a partial blob and its files, for example when its size turned out
// to be wrong. Handles on it must be closed first. Discarding a missing partial is a no-op.
func (s *Store) DiscardPartial(m cid.Multihash) error {
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	if _, err := s.db.Exec(`DELETE FROM partials WHERE mh = ?`, mhKey(m)); err != nil {
		return err
	}
	path := s.partialPath(m)
	for _, f := range []string{path, path + ".obao"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Sink returns the blob.Sink for blob.DecodeSlice; it may be used from several goroutines
// for disjoint ranges.
func (p *Partial) Sink() blob.Sink {
	return blob.Sink{Data: p.f, Outboard: p.ob, Group: p.mark}
}

func (p *Partial) mark(g uint64) {
	p.mu.Lock()
	p.groups[g/8] |= 1 << (g % 8)
	p.dirty = true
	p.mu.Unlock()
}

func (p *Partial) has(g uint64) bool { return p.groups[g/8]&(1<<(g%8)) != 0 }

// Held returns the verified group ranges, in order.
func (p *Partial) Held() []blob.Range { return p.ranges(true) }

// Missing returns the group ranges still to fetch, in order.
func (p *Partial) Missing() []blob.Range { return p.ranges(false) }

func (p *Partial) ranges(held bool) []blob.Range {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []blob.Range
	n := blob.Groups(p.Size)
	for g := uint64(0); g < n; g++ {
		if p.has(g) != held {
			continue
		}
		if len(out) > 0 && out[len(out)-1].End == g {
			out[len(out)-1].End++
		} else {
			out = append(out, blob.Range{Start: g, End: g + 1})
		}
	}
	return out
}

// Has reports whether every group in r is held.
func (p *Partial) Has(r blob.Range) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for g := r.Start; g < r.End; g++ {
		if !p.has(g) {
			return false
		}
	}
	return r.Valid(p.Size)
}

// Complete reports whether every group is held.
func (p *Partial) Complete() bool { return len(p.Missing()) == 0 }

// ReadAt reads held bytes; callers must check Has first.
func (p *Partial) ReadAt(b []byte, off int64) (int, error) { return p.f.ReadAt(b, off) }

// ExtractSlice serves a held range onward before the blob is complete.
func (p *Partial) ExtractSlice(dst io.Writer, r blob.Range) error {
	if !p.Has(r) {
		return fmt.Errorf("store: range %v not held", r)
	}
	return blob.ExtractSlice(dst, p.f, p.ob, p.Size, r)
}

// Flush syncs the data and persists the held-group map and verified byte count.
func (p *Partial) Flush() error {
	p.mu.Lock()
	if !p.dirty || p.readOnly {
		p.mu.Unlock()
		return nil
	}
	snap := append([]byte(nil), p.groups...)
	p.dirty = false
	p.mu.Unlock()
	if err := p.f.Sync(); err != nil {
		return err
	}
	if err := p.ob.Sync(); err != nil {
		return err
	}
	held := heldBytes(snap, p.Size)
	_, err := p.s.db.Exec(`UPDATE partials SET groups = ?, held = ?, updated = ? WHERE mh = ?`, snap, held, p.s.nowUnix(), mhKey(p.MH))
	if err != nil {
		return err
	}
	// The persisted bytes are now counted; shrink the reservation by the same amount.
	p.mu.Lock()
	delta := min(held-p.held, p.reserved)
	p.held = held
	p.reserved -= delta
	p.mu.Unlock()
	p.s.reserve(-delta)
	return nil
}

// Commit turns a complete partial into a normal blob, atomically.
func (p *Partial) Commit() error {
	if !p.Complete() {
		return errors.New("store: partial blob is not complete")
	}
	if err := p.Flush(); err != nil {
		return err
	}
	if err := p.f.Truncate(int64(p.Size)); err != nil {
		return err
	}
	if err := p.f.Sync(); err != nil {
		return err
	}
	path := p.s.partialPath(p.MH)
	if err := p.s.commitBlobTx(p.MH, p.Size, path, path+".obao", "", 0, true); err != nil {
		return err
	}
	return p.close()
}

// Close flushes and closes the handle; the partial blob remains for resumption.
func (p *Partial) Close() error {
	return errors.Join(p.Flush(), p.close())
}

func (p *Partial) close() error {
	p.release()
	return errors.Join(p.f.Close(), p.ob.Close())
}

// HeldRanges reports what the store holds of a blob, for /nu/have/1: the whole range for a
// complete blob, verified ranges for a partial one.
func (s *Store) HeldRanges(m cid.Multihash) (size uint64, held []blob.Range, complete bool, err error) {
	info, ok, err := s.Has(cid.New(cid.Raw, m))
	if err != nil || !ok {
		return 0, nil, false, err
	}
	if info.Complete {
		return info.Size, []blob.Range{{Start: 0, End: blob.Groups(info.Size)}}, true, nil
	}
	var groups []byte
	if err := s.db.QueryRow(`SELECT groups FROM partials WHERE mh = ?`, mhKey(m)).Scan(&groups); err != nil {
		return 0, nil, false, err
	}
	p := &Partial{MH: m, Size: info.Size, groups: groups}
	return info.Size, p.Held(), false, nil
}
