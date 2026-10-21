// Package store is the local store (D17): blobs and Bao outboards as files keyed by
// multihash, SQLite (modernc.org/sqlite) for DRISL blocks, small inline blobs, partial-blob
// range maps and pins, plus mark-and-sweep GC and --nocopy references.
//
// Layout of a store directory (all files 0600, directories 0700):
//
//	nu.db                 SQLite metadata (schema version in PRAGMA user_version)
//	blobs/<xx>/<mh-hex>   complete blob data; <mh-hex>.obao is its Bao outboard
//	partial/<mh-hex>      partially fetched BLAKE3 blob (sparse) and .obao
//	staging/              in-progress writes; emptied on Open
//
// An object becomes visible only when its metadata row commits, after its files were
// written, synced and renamed into place; a crash leaves at most orphan files, which GC
// removes.
package store

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
)

// ErrNotFound is returned for objects the store does not hold (it wraps mst.ErrNotFound so
// the store can back MST/tree reads).
var ErrNotFound = fmt.Errorf("store: %w", mst.ErrNotFound)

// Options configures Open.
type Options struct {
	// DefaultHash is the hash for AddBlob: cid.BLAKE3 (default) or cid.SHA256.
	DefaultHash cid.Hash
}

// Store is safe for concurrent use.
type Store struct {
	dir      string
	db       *sql.DB
	opts     Options
	gcMu     sync.RWMutex // writers commit under RLock; GC holds Lock
	profMu   sync.RWMutex
	profiles map[string]LinksFunc
	now      func() time.Time
}

// LinksFunc is a walk profile: the links of a block that a recursive pin follows.
type LinksFunc func(c cid.CID, data []byte) ([]cid.CID, error)

// Open opens or creates a store in dir, applying schema migrations.
func Open(dir string, opts Options) (*Store, error) {
	if opts.DefaultHash == 0 {
		opts.DefaultHash = cid.BLAKE3
	}
	for _, d := range []string{dir, filepath.Join(dir, "blobs"), filepath.Join(dir, "partial"), filepath.Join(dir, "staging")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	// Abandoned in-progress writes are never visible; drop them.
	if ents, err := os.ReadDir(filepath.Join(dir, "staging")); err == nil {
		for _, e := range ents {
			_ = os.RemoveAll(filepath.Join(dir, "staging", e.Name()))
		}
	}
	dbPath := filepath.Join(dir, "nu.db")
	if f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0o600); err != nil {
		return nil, err
	} else {
		f.Close()
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{dir: dir, db: db, opts: opts, now: time.Now, profiles: map[string]LinksFunc{
		"nu-tree": tree.Links,
	}}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// RegisterProfile adds a walk profile for recursive pins (e.g. "atrepo", "drisl").
func (s *Store) RegisterProfile(name string, fn LinksFunc) {
	s.profMu.Lock()
	defer s.profMu.Unlock()
	s.profiles[name] = fn
}

func (s *Store) profile(name string) (LinksFunc, bool) {
	s.profMu.RLock()
	defer s.profMu.RUnlock()
	fn, ok := s.profiles[name]
	return fn, ok
}

// migrations[i] upgrades schema version i to i+1. Append only; never edit a released entry.
var migrations = []string{
	// 1: blocks, blobs, partial range maps, pins.
	`CREATE TABLE blocks (
		mh BLOB PRIMARY KEY, codec INTEGER NOT NULL, data BLOB NOT NULL, created INTEGER NOT NULL
	) WITHOUT ROWID;
	CREATE TABLE blobs (
		mh BLOB PRIMARY KEY, size INTEGER NOT NULL, outboard INTEGER NOT NULL,
		src_path TEXT, src_size INTEGER, src_mtime INTEGER, created INTEGER NOT NULL
	) WITHOUT ROWID;
	CREATE TABLE partials (
		mh BLOB PRIMARY KEY, size INTEGER NOT NULL, groups BLOB NOT NULL, updated INTEGER NOT NULL
	) WITHOUT ROWID;
	CREATE TABLE pins (
		cid BLOB NOT NULL, mode TEXT NOT NULL, profile TEXT NOT NULL, source TEXT NOT NULL,
		name TEXT NOT NULL, created INTEGER NOT NULL, PRIMARY KEY (cid, mode, source, name)
	);`,
	// 2: import cache: (path, hash) → blob CID with the source's size/mtime/inode, so an
	// unchanged file is not re-hashed on re-add.
	`CREATE TABLE import_cache (
		path TEXT NOT NULL, hash INTEGER NOT NULL, size INTEGER NOT NULL, mtime INTEGER NOT NULL,
		inode INTEGER NOT NULL, cid BLOB NOT NULL, used INTEGER NOT NULL, PRIMARY KEY (path, hash)
	) WITHOUT ROWID;`,
}

// SchemaVersion is the schema version this binary writes.
var SchemaVersion = len(migrations)

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("store: database schema version %d is newer than this binary supports (%d); upgrade nu", v, len(migrations))
	}
	for ; v < len(migrations); v++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", v+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func mhKey(m cid.Multihash) []byte { return m.Bytes() }

func mhHex(m cid.Multihash) string { return hex.EncodeToString(m.Bytes()) }

func (s *Store) blobPath(m cid.Multihash) string {
	h := mhHex(m)
	return filepath.Join(s.dir, "blobs", h[4:6], h)
}

func (s *Store) partialPath(m cid.Multihash) string {
	return filepath.Join(s.dir, "partial", mhHex(m))
}

func (s *Store) stagingFile(pattern string) (*os.File, error) {
	return os.CreateTemp(filepath.Join(s.dir, "staging"), pattern) // mode 0600
}

func (s *Store) nowUnix() int64 { return s.now().UnixNano() }

// Info describes a held object.
type Info struct {
	Codec    cid.Codec // as first stored; the bytes are keyed by multihash
	Size     uint64
	Complete bool // false for partial blobs
	NoCopy   bool
}

// Has reports whether the store holds the object with c's multihash. A --nocopy blob whose
// source changed is re-verified and, if it no longer matches, reported absent.
func (s *Store) Has(c cid.CID) (Info, bool, error) {
	m := c.Multihash()
	var codec int
	var size int64
	err := s.db.QueryRow(`SELECT codec, length(data) FROM blocks WHERE mh = ?`, mhKey(m)).Scan(&codec, &size)
	if err == nil {
		return Info{Codec: cid.Codec(codec), Size: uint64(size), Complete: true}, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Info{}, false, err
	}
	row, err := s.blobRow(m)
	if err == nil {
		if row.srcPath != "" {
			if ok, err := s.checkNoCopy(row); err != nil || !ok {
				return Info{}, false, err
			}
		}
		return Info{Codec: cid.Raw, Size: row.size, Complete: true, NoCopy: row.srcPath != ""}, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Info{}, false, err
	}
	err = s.db.QueryRow(`SELECT size FROM partials WHERE mh = ?`, mhKey(m)).Scan(&size)
	if err == nil {
		return Info{Codec: cid.Raw, Size: uint64(size)}, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Info{}, false, nil
	}
	return Info{}, false, err
}

// Get returns a block held in the database: DRISL structure or a small inline blob. It
// implements mst.Store together with Put.
func (s *Store) Get(c cid.CID) ([]byte, error) {
	var data []byte
	err := s.db.QueryRow(`SELECT data FROM blocks WHERE mh = ?`, mhKey(c.Multihash())).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, c)
	}
	return data, err
}

// Put verifies and stores a whole object. Large BLAKE3 raw blobs go to the blob store.
func (s *Store) Put(c cid.CID, data []byte) error {
	if err := c.Verify(data); err != nil {
		return err
	}
	if c.Codec() == cid.Raw && len(data) > inlineMax {
		_, err := s.PutBlob(c, bytesReader(data), int64(len(data)))
		return err
	}
	return s.putBlock(c.Codec(), c.Multihash(), data)
}

func (s *Store) putBlock(codec cid.Codec, m cid.Multihash, data []byte) error {
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	_, err := s.db.Exec(`INSERT OR IGNORE INTO blocks (mh, codec, data, created) VALUES (?, ?, ?, ?)`,
		mhKey(m), int(codec), data, s.nowUnix())
	return err
}

// Usage returns the bytes the store holds: blocks, copied blobs, outboards and partials.
func (s *Store) Usage() (int64, error) {
	var blocks, blobs, obs, parts sql.NullInt64
	err := s.db.QueryRow(`SELECT
		(SELECT sum(length(data)) FROM blocks),
		(SELECT sum(size) FROM blobs WHERE src_path IS NULL),
		(SELECT sum(8 + 64 * ((size + 16383) / 16384 - 1)) FROM blobs WHERE outboard = 1),
		(SELECT sum(size) FROM partials)`).Scan(&blocks, &blobs, &obs, &parts)
	return blocks.Int64 + blobs.Int64 + obs.Int64 + parts.Int64, err
}
