package store

import (
	"errors"
	"io"
	"os"
	"sync"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
)

// Batch stages blob adds whose durability is settled together (0084). Its AddBlobHash and
// AddNoCopy hash and write like the Store's, but leave the fsyncs, the rename into place
// and the row insert to Commit. Commit syncs every staged file in parallel, so the
// filesystem can merge the flushes, and then makes all the blobs visible in one
// transaction. A blob is never visible before its bytes are durable, and a crash before
// Commit leaves only staging files, which Open removes. Blobs that fit inline go straight
// to the database as with the Store. The returned CIDs are held only once Commit returns.
// A Batch keeps its staged files open; commit at a threshold (Len, Bytes). It is not safe
// for concurrent use.
type Batch struct {
	s       *Store
	pending []stagedBlob
	bytes   int64
}

// stagedBlob is a blob written to staging files and waiting for Commit. data is nil for
// --nocopy blobs; ob is nil for blobs without an outboard.
type stagedBlob struct {
	m        cid.Multihash
	size     uint64
	data, ob *os.File
	srcPath  string
	srcMtime int64
}

// syncParallel bounds the concurrent fsyncs of a Commit. Concurrent fsyncs share journal
// commits and device flushes. On the alphac NVMe drives, 1000 files took 6 s one at a time
// and under 0.1 s with 128 concurrent fsyncs (docs/validation.md "LAN benchmarks").
const syncParallel = 64

// Batch starts an empty batch.
func (s *Store) Batch() *Batch { return &Batch{s: s} }

// AddBlobHash is Store.AddBlobHash, staged in the batch.
func (b *Batch) AddBlobHash(r io.Reader, size int64, h cid.Hash) (cid.CID, error) {
	return b.s.addBlob(r, size, h, nil, b)
}

// AddNoCopy is Store.AddNoCopy, staged in the batch.
func (b *Batch) AddNoCopy(path string) (cid.CID, error) { return b.s.addNoCopy(path, b) }

// Len is the number of staged blobs; Bytes their staged file bytes.
func (b *Batch) Len() int     { return len(b.pending) }
func (b *Batch) Bytes() int64 { return b.bytes }

func (b *Batch) stage(p stagedBlob) {
	b.pending = append(b.pending, p)
	if p.data != nil {
		b.bytes += int64(p.size)
	}
	if p.ob != nil {
		b.bytes += int64(blob.OutboardSize(p.size))
	}
}

// Commit makes every staged blob durable and visible, then empties the batch. On error
// nothing staged becomes visible, and the batch is empty too.
func (b *Batch) Commit() error {
	defer b.Abort()
	if len(b.pending) == 0 {
		return nil
	}
	var files []*os.File
	for _, p := range b.pending {
		for _, f := range []*os.File{p.data, p.ob} {
			if f != nil {
				files = append(files, f)
			}
		}
	}
	if err := SyncFiles(files); err != nil {
		return err
	}
	s := b.s
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range b.pending {
		if err := s.commitBlobIn(tx, p.m, p.size, fileName(p.data), fileName(p.ob), p.srcPath, p.srcMtime, false); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Abort closes and removes whatever is still staged and empties the batch. After a Commit
// the staged names have been renamed away, so only the closes remain.
func (b *Batch) Abort() {
	for _, p := range b.pending {
		for _, f := range []*os.File{p.data, p.ob} {
			if f != nil {
				f.Close()
				os.Remove(f.Name())
			}
		}
	}
	b.pending, b.bytes = nil, 0
}

// SyncFiles fsyncs files concurrently (at most syncParallel at a time) and joins the
// errors.
func SyncFiles(files []*os.File) error {
	errs := make([]error, len(files))
	sem := make(chan struct{}, syncParallel)
	var wg sync.WaitGroup
	for i, f := range files {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = f.Sync()
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}
