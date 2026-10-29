package store

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
)

// inlineMax is the largest raw blob kept inline in the database (one chunk group, the
// largest blob without an outboard).
const inlineMax = blob.GroupSize

// noOutboardMax is the largest copied BLAKE3 blob stored without an outboard file: one
// fetch request (64 groups), always served whole, so a server builds its outboard in
// memory instead of every small file costing a second file, fsync and rename.
const noOutboardMax = 64 * blob.GroupSize

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// AddBlob streams size bytes from r into the store under the default hash and returns the
// raw CID (tree.BlobSink). size -1 means unknown: the data is staged first and the outboard
// computed in a second pass.
func (s *Store) AddBlob(r io.Reader, size int64) (cid.CID, error) {
	return s.addBlob(r, size, s.opts.DefaultHash, nil, nil)
}

// AddBlobHash is AddBlob with an explicit hash. SHA-256 blobs are limited to 8 MiB.
func (s *Store) AddBlobHash(r io.Reader, size int64, h cid.Hash) (cid.CID, error) {
	return s.addBlob(r, size, h, nil, nil)
}

// PutBlob stores a blob fetched by known CID, verifying it as a whole. For large BLAKE3
// blobs fetched by range, use Partial instead.
func (s *Store) PutBlob(c cid.CID, r io.Reader, size int64) (cid.CID, error) {
	return s.addBlob(r, size, c.Hash(), &c, nil)
}

// addBlob stores a blob; with a batch, a blob that needs files is staged in it instead of
// being synced and committed now.
func (s *Store) addBlob(r io.Reader, size int64, h cid.Hash, want *cid.CID, b *Batch) (cid.CID, error) {
	if !h.Valid() {
		return cid.CID{}, cid.ErrInvalid
	}
	if h == cid.SHA256 && size > cid.MaxWholeObject {
		return cid.CID{}, cid.ErrTooLarge
	}
	check := func(m cid.Multihash) (cid.CID, error) {
		c := cid.New(cid.Raw, m)
		if want != nil && want.Multihash() != m {
			return cid.CID{}, fmt.Errorf("store: %w (want %s, got %s)", cid.ErrMismatch, want, c)
		}
		return c, nil
	}
	if size >= 0 && size <= inlineMax {
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return cid.CID{}, err
		}
		c, err := check(cid.Sum(h, data))
		if err != nil {
			return cid.CID{}, err
		}
		return c, s.putInline(c.Multihash(), data, b)
	}

	// Admission (A6): a known size needs the data plus its outboard; an unknown size
	// needs one chunk group now and its actual bytes at commit.
	need := int64(inlineMax)
	if size >= 0 {
		need = size + int64(blob.OutboardSize(uint64(size)))
	}
	if err := s.admit(need); err != nil {
		return cid.CID{}, err
	}
	f, err := s.stagingFile("blob-*")
	if err != nil {
		return cid.CID{}, err
	}
	var ob *os.File
	staged := false // handed to the batch, which closes and removes the files
	defer func() {
		if staged {
			return
		}
		f.Close()
		os.Remove(f.Name())
		if ob != nil {
			ob.Close()
			os.Remove(ob.Name())
		}
	}()

	var m cid.Multihash
	if h == cid.BLAKE3 && size > noOutboardMax {
		// One pass: data to the staging file while the outboard is built.
		if ob, err = s.stagingFile("obao-*"); err != nil {
			return cid.CID{}, err
		}
		if m, err = blob.Outboard(ob, io.TeeReader(io.LimitReader(r, size), f), uint64(size)); err != nil {
			return cid.CID{}, err
		}
	} else {
		limit := int64(1<<63 - 1)
		if h == cid.SHA256 {
			limit = cid.MaxWholeObject + 1
		}
		hs := h.New()
		n, err := io.Copy(io.MultiWriter(f, hs), io.LimitReader(r, limit))
		if err != nil {
			return cid.CID{}, err
		}
		if h == cid.SHA256 && n > cid.MaxWholeObject {
			return cid.CID{}, cid.ErrTooLarge
		}
		if size >= 0 && n != size {
			return cid.CID{}, io.ErrUnexpectedEOF
		}
		size = n
		m = cid.FromHasher(h, hs)
		if size <= inlineMax {
			data, err := os.ReadFile(f.Name())
			if err != nil {
				return cid.CID{}, err
			}
			c, err := check(m)
			if err != nil {
				return cid.CID{}, err
			}
			return c, s.putInline(m, data, b)
		}
		if h == cid.BLAKE3 && size > noOutboardMax {
			// The staged bytes are counted; the outboard is still to come.
			if err := s.admit(int64(blob.OutboardSize(uint64(size)))); err != nil {
				return cid.CID{}, err
			}
			if ob, err = s.stagingFile("obao-*"); err != nil {
				return cid.CID{}, err
			}
			if _, err := blob.Outboard(ob, io.NewSectionReader(f, 0, size), uint64(size)); err != nil {
				return cid.CID{}, err
			}
		}
	}
	c, err := check(m)
	if err != nil {
		return cid.CID{}, err
	}
	if b != nil {
		staged = true
		b.stage(stagedBlob{m: m, size: uint64(size), data: f, ob: ob})
		return c, nil
	}
	if err := f.Sync(); err != nil {
		return cid.CID{}, err
	}
	if ob != nil {
		if err := ob.Sync(); err != nil {
			return cid.CID{}, err
		}
	}
	return c, s.commitBlob(m, uint64(size), f.Name(), fileName(ob), "", 0)
}

// putInline stores a raw blob of at most inlineMax bytes in the database, or stages it in
// b for b's commit transaction.
func (s *Store) putInline(m cid.Multihash, data []byte, b *Batch) error {
	if b == nil {
		return s.putBlock(cid.Raw, m, data)
	}
	if err := s.admit(int64(len(data))); err != nil {
		return err
	}
	b.blocks = append(b.blocks, stagedBlock{m: m, data: data})
	b.bytes += int64(len(data))
	return nil
}

func fileName(f *os.File) string {
	if f == nil {
		return ""
	}
	return f.Name()
}

// commitBlob renames staged files into place and then makes the blob visible by inserting
// its row. srcPath is set for --nocopy blobs (dataFile is then empty).
func (s *Store) commitBlob(m cid.Multihash, size uint64, dataFile, obFile, srcPath string, srcMtime int64) error {
	return s.commitBlobTx(m, size, dataFile, obFile, srcPath, srcMtime, false)
}

// commitBlobTx is commitBlob; fromPartial also drops the partial row in the same
// transaction.
func (s *Store) commitBlobTx(m cid.Multihash, size uint64, dataFile, obFile, srcPath string, srcMtime int64, fromPartial bool) error {
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.commitBlobIn(tx, m, size, dataFile, obFile, srcPath, srcMtime, fromPartial); err != nil {
		return err
	}
	return tx.Commit()
}

// txRunner is a *sql.Tx or a preparedTx over one.
type txRunner interface {
	Exec(q string, args ...any) (sql.Result, error)
	QueryRow(q string, args ...any) *sql.Row
}

// commitBlobIn is commitBlobTx's work inside tx (Batch.Commit runs it for many blobs).
// Callers hold gcMu.RLock. Staged files of a blob already held as a full copy are left
// for the caller to remove.
func (s *Store) commitBlobIn(tx txRunner, m cid.Multihash, size uint64, dataFile, obFile, srcPath string, srcMtime int64, fromPartial bool) error {
	var held sql.NullString
	switch err := tx.QueryRow(`SELECT src_path FROM blobs WHERE mh = ?`, mhKey(m)).Scan(&held); {
	case err == nil && held.String == "":
		return nil // already held as a full copy
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	}
	final := s.blobPath(m)
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return err
	}
	if obFile != "" {
		if err := os.Rename(obFile, final+".obao"); err != nil {
			return err
		}
	}
	if dataFile != "" {
		if err := os.Rename(dataFile, final); err != nil {
			return err
		}
	}
	var src any
	if srcPath != "" {
		src = srcPath
	}
	// Delete then insert (not REPLACE) so the usage triggers see both rows.
	if _, err := tx.Exec(`DELETE FROM blobs WHERE mh = ?`, mhKey(m)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO blobs (mh, size, outboard, src_path, src_size, src_mtime, created)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, mhKey(m), int64(size), obFile != "", src, int64(size), srcMtime, s.nowUnix()); err != nil {
		return err
	}
	if fromPartial {
		if _, err := tx.Exec(`DELETE FROM partials WHERE mh = ?`, mhKey(m)); err != nil {
			return err
		}
	}
	return nil
}

type blobRow struct {
	m        cid.Multihash
	size     uint64
	outboard bool
	srcPath  string
	srcMtime int64
}

func (s *Store) blobRow(m cid.Multihash) (blobRow, error) {
	r := blobRow{m: m}
	var size int64
	var src sql.NullString
	var mtime sql.NullInt64
	err := s.queryRow(`SELECT size, outboard, src_path, src_mtime FROM blobs WHERE mh = ?`, mhKey(m)).
		Scan(&size, &r.outboard, &src, &mtime)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	r.size, r.srcPath, r.srcMtime = uint64(size), src.String, mtime.Int64
	return r, err
}

// Blob is an open complete blob. It is an io.ReaderAt over the blob bytes.
type Blob struct {
	MH   cid.Multihash
	Size uint64
	data io.ReaderAt
	ob   io.ReaderAt
	cls  []io.Closer
}

func (b *Blob) ReadAt(p []byte, off int64) (int, error) { return b.data.ReadAt(p, off) }

// Reader returns a sequential reader over the whole blob.
func (b *Blob) Reader() io.Reader { return io.NewSectionReader(b.data, 0, int64(b.Size)) }

// Outboard returns the Bao outboard, or nil for blobs without one.
func (b *Blob) Outboard() io.ReaderAt { return b.ob }

// ExtractSlice writes the Bao slice for range r (BLAKE3 blobs with an outboard).
func (b *Blob) ExtractSlice(dst io.Writer, r blob.Range) error {
	if b.ob == nil {
		return errors.New("store: blob has no outboard; send it whole")
	}
	return blob.ExtractSlice(dst, b.data, b.ob, b.Size, r)
}

func (b *Blob) Close() error {
	var err error
	for _, c := range b.cls {
		err = errors.Join(err, c.Close())
	}
	return err
}

// OpenBlob opens a complete raw blob (inline or file) by multihash.
func (s *Store) OpenBlob(m cid.Multihash) (*Blob, error) {
	var data []byte
	err := s.queryRow(`SELECT data FROM blocks WHERE mh = ?`, mhKey(m)).Scan(&data)
	if err == nil {
		return &Blob{MH: m, Size: uint64(len(data)), data: bytes.NewReader(data)}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	row, err := s.blobRow(m)
	if err != nil {
		return nil, err
	}
	path := s.blobPath(m)
	if row.srcPath != "" {
		ok, err := s.checkNoCopy(row)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: --nocopy source %s changed", ErrNotFound, row.srcPath)
		}
		path = row.srcPath
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	b := &Blob{MH: m, Size: row.size, data: f, cls: []io.Closer{f}}
	if row.outboard {
		o, err := os.Open(s.blobPath(m) + ".obao")
		if err != nil {
			f.Close()
			return nil, err
		}
		b.ob, b.cls = o, append(b.cls, o)
	}
	return b, nil
}

// AddNoCopy references the file at path in place (BLAKE3; the outboard is stored). Reads
// check its size and mtime and re-hash it on mismatch; a source that no longer matches is
// treated as absent. Files of at most one chunk group are copied instead.
func (s *Store) AddNoCopy(path string) (cid.CID, error) { return s.addNoCopy(path, nil) }

func (s *Store) addNoCopy(path string, b *Batch) (cid.CID, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return cid.CID{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return cid.CID{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return cid.CID{}, err
	}
	if !st.Mode().IsRegular() {
		return cid.CID{}, fmt.Errorf("store: %s is not a regular file", path)
	}
	size := st.Size()
	if size <= inlineMax {
		return s.addBlob(f, size, cid.BLAKE3, nil, b)
	}
	if err := s.admit(int64(blob.OutboardSize(uint64(size)))); err != nil {
		return cid.CID{}, err
	}
	ob, err := s.stagingFile("obao-*")
	if err != nil {
		return cid.CID{}, err
	}
	staged := false
	defer func() {
		if !staged {
			ob.Close()
			os.Remove(ob.Name())
		}
	}()
	m, err := blob.Outboard(ob, f, uint64(size))
	if err != nil {
		return cid.CID{}, err
	}
	// The source must not have changed while hashing.
	if st2, err := os.Stat(path); err != nil || st2.Size() != size || !st2.ModTime().Equal(st.ModTime()) {
		return cid.CID{}, fmt.Errorf("store: %s changed while it was being added", path)
	}
	if b != nil {
		staged = true
		b.stage(stagedBlob{m: m, size: uint64(size), ob: ob, srcPath: path, srcMtime: st.ModTime().UnixNano()})
		return cid.New(cid.Raw, m), nil
	}
	if err := ob.Sync(); err != nil {
		return cid.CID{}, err
	}
	return cid.New(cid.Raw, m), s.commitBlob(m, uint64(size), "", ob.Name(), path, st.ModTime().UnixNano())
}

// checkNoCopy verifies a --nocopy source: a size/mtime match is trusted; otherwise the file
// is re-hashed. A source that no longer matches loses its row (and outboard), so it is not
// advertised until re-added or fetched.
func (s *Store) checkNoCopy(row blobRow) (bool, error) {
	st, err := os.Stat(row.srcPath)
	if err == nil && uint64(st.Size()) == row.size && st.ModTime().UnixNano() == row.srcMtime {
		return true, nil
	}
	if err == nil && uint64(st.Size()) == row.size {
		if f, err := os.Open(row.srcPath); err == nil {
			mh, n, herr := blob.Sum(f)
			f.Close()
			if herr == nil && n == row.size && mh == row.m {
				_, err := s.db.Exec(`UPDATE blobs SET src_mtime = ? WHERE mh = ?`, st.ModTime().UnixNano(), mhKey(row.m))
				return true, err
			}
		}
	}
	// No gcMu here: Has runs inside GC's mark phase.
	if _, err := s.db.Exec(`DELETE FROM blobs WHERE mh = ? AND src_path = ?`, mhKey(row.m), row.srcPath); err != nil {
		return false, err
	}
	s.removals.Add(1)
	_ = os.Remove(s.blobPath(row.m) + ".obao")
	return false, nil
}
