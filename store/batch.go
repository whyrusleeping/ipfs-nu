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
// Commit leaves only staging files, which Open removes. Blobs that fit inline and import
// cache entries are kept in memory and written in the same transaction, so a batch of
// small files costs one database commit rather than two per file. The returned CIDs are
// held only once Commit returns. A Batch keeps its staged files open and its inline blobs
// in memory; commit at a threshold (Len, Bytes). It is not safe for concurrent use.
type Batch struct {
	s       *Store
	pending []stagedBlob
	blocks  []stagedBlock
	cache   []stagedCache
	bytes   int64
}

// stagedBlock is an inline raw blob waiting for Commit.
type stagedBlock struct {
	m    cid.Multihash
	data []byte
}

// stagedCache is an import cache entry waiting for Commit.
type stagedCache struct {
	src Source
	h   cid.Hash
	c   cid.CID
}

// stagedBlob is a blob written to staging files and waiting for Commit. data is nil for
// --nocopy blobs; ob is nil for blobs without an outboard.
type stagedBlob struct {
	m        cid.Multihash
	size     uint64
	data, ob *os.File
	srcPath  string
	srcMtime int64
	synced   bool // by Batch.Sync
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

// ImportCachePut is Store.ImportCachePut, written at Commit.
func (b *Batch) ImportCachePut(src Source, h cid.Hash, c cid.CID) {
	b.cache = append(b.cache, stagedCache{src, h, c})
}

// Len is the number of staged blobs; Bytes their staged file and inline bytes.
func (b *Batch) Len() int     { return len(b.pending) + len(b.blocks) }
func (b *Batch) Bytes() int64 { return b.bytes }

// Sync makes the files staged so far durable now, so that Commit does not sync them.
// Batches synced by their own goroutines and then Appended commit together as one
// transaction without serializing their fsyncs.
func (b *Batch) Sync() error {
	var files []*os.File
	for _, p := range b.pending {
		if !p.synced {
			files = append(files, p.files()...)
		}
	}
	if err := SyncFiles(files); err != nil {
		return err
	}
	for i := range b.pending {
		b.pending[i].synced = true
	}
	return nil
}

// Append moves everything staged in o into b, leaving o empty.
func (b *Batch) Append(o *Batch) {
	b.pending = append(b.pending, o.pending...)
	b.blocks = append(b.blocks, o.blocks...)
	b.cache = append(b.cache, o.cache...)
	b.bytes += o.bytes
	o.pending, o.blocks, o.cache, o.bytes = nil, nil, nil, 0
}

func (p stagedBlob) files() []*os.File {
	var out []*os.File
	for _, f := range []*os.File{p.data, p.ob} {
		if f != nil {
			out = append(out, f)
		}
	}
	return out
}

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
	if len(b.pending) == 0 && len(b.blocks) == 0 && len(b.cache) == 0 {
		return nil
	}
	if err := b.Sync(); err != nil {
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
	ptx := newPreparedTx(tx)
	defer ptx.close()
	now := s.nowUnix()
	for _, p := range b.blocks {
		if _, err := ptx.Exec(insertBlock, mhKey(p.m), int(cid.Raw), p.data, now); err != nil {
			return err
		}
	}
	for _, p := range b.pending {
		if err := s.commitBlobIn(ptx, p.m, p.size, fileName(p.data), fileName(p.ob), p.srcPath, p.srcMtime, false); err != nil {
			return err
		}
	}
	for _, e := range b.cache {
		if _, err := ptx.Exec(importCachePut, e.src.Path, int(e.h), e.src.Size, e.src.Mtime, e.src.Inode, e.c.Bytes(), now); err != nil {
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
	b.pending, b.blocks, b.cache, b.bytes = nil, nil, nil, 0
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
