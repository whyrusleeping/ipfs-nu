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
//
// Accounting (D17 A1–A8, accounting.go): physical bytes per class are counters kept by
// triggers, every write is admitted against Limits before it stores anything, and
// GCIfNeeded collects at the watermark or below the free-space reserve.
package store

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/car"
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
	// Limits are the budgets (A5); see Limits for the zero values.
	Limits Limits
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

	limMu  sync.RWMutex
	limits Limits
	// resMu guards the admission reservations and the pressure-GC clock.
	resMu          sync.Mutex
	reserved       int64
	admitted       admitCache
	lastPressureGC time.Time
	// removals counts committed removals of held objects (Removals, many.go).
	removals atomic.Uint64
	// stmts caches the prepared per-object queries (queryRow, exec).
	stmtMu sync.Mutex
	stmts  map[string]*sql.Stmt
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
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)&_pragma=temp_store(MEMORY)&_pragma=cache_size(-32768)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := enableIncrementalVacuum(db); err != nil { // vacuum.go
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{dir: dir, db: db, opts: opts, now: time.Now, limits: opts.Limits.withDefaults(), profiles: map[string]LinksFunc{
		"nu-tree":      tree.Links,
		"nu-catalog":   tree.CatalogLinks,
		"atrepo":       atrepo.Links,
		"atrepo+blobs": atrepo.LinksWithBlobs,
		"drisl":        car.DRISLLinks,
	}}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	s.stmtMu.Lock()
	for _, st := range s.stmts {
		st.Close()
	}
	s.stmts = nil
	s.stmtMu.Unlock()
	return s.db.Close()
}

// stmt returns q prepared once per store. The per-object queries run once per file of a
// tree, and parsing them each time cost about as much as running them.
func (s *Store) stmt(q string) (*sql.Stmt, error) {
	s.stmtMu.Lock()
	defer s.stmtMu.Unlock()
	if st := s.stmts[q]; st != nil {
		return st, nil
	}
	st, err := s.db.Prepare(q)
	if err != nil {
		return nil, err
	}
	if s.stmts == nil {
		s.stmts = map[string]*sql.Stmt{}
	}
	s.stmts[q] = st
	return st, nil
}

// queryRow is db.QueryRow with a cached prepared statement.
func (s *Store) queryRow(q string, args ...any) *sql.Row {
	st, err := s.stmt(q)
	if err != nil {
		return s.db.QueryRow(q, args...) // reports the error through Scan
	}
	return st.QueryRow(args...)
}

// exec is db.Exec with a cached prepared statement.
func (s *Store) exec(q string, args ...any) (sql.Result, error) {
	st, err := s.stmt(q)
	if err != nil {
		return nil, err
	}
	return st.Exec(args...)
}

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
// A migration is SQL plus an optional Go step run in the same transaction after it.
type migration struct {
	sql  string
	post func(tx *sql.Tx) error
}

var migrations = []migration{
	// 1: blocks, blobs, partial range maps, pins.
	{sql: `CREATE TABLE blocks (
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
	);`},
	// 2: import cache: (path, hash) → blob CID with the source's size/mtime/inode, so an
	// unchanged file is not re-hashed on re-add.
	{sql: `CREATE TABLE import_cache (
		path TEXT NOT NULL, hash INTEGER NOT NULL, size INTEGER NOT NULL, mtime INTEGER NOT NULL,
		inode INTEGER NOT NULL, cid BLOB NOT NULL, used INTEGER NOT NULL, PRIMARY KEY (path, hash)
	) WITHOUT ROWID;`},
	// 3: accounting (D17 A1): the verified bytes of each partial blob, and per-class
	// physical byte counters maintained by triggers in the writing transaction. The
	// outboard formula is blob.OutboardSize for sizes above one group.
	{sql: `ALTER TABLE partials ADD COLUMN held INTEGER NOT NULL DEFAULT 0;
	CREATE TABLE usage (class TEXT PRIMARY KEY, bytes INTEGER NOT NULL) WITHOUT ROWID;
	INSERT INTO usage VALUES ('blocks', (SELECT coalesce(sum(length(data)), 0) FROM blocks));
	INSERT INTO usage VALUES ('blobs', (SELECT coalesce(sum(size), 0) FROM blobs WHERE src_path IS NULL));
	INSERT INTO usage VALUES ('nocopy', (SELECT coalesce(sum(size), 0) FROM blobs WHERE src_path IS NOT NULL));
	INSERT INTO usage VALUES ('outboards',
		(SELECT coalesce(sum(8 + 64 * ((size + 16383) / 16384 - 1)), 0) FROM blobs WHERE outboard = 1)
		+ (SELECT coalesce(sum(8 + 64 * ((size + 16383) / 16384 - 1)), 0) FROM partials));
	INSERT INTO usage VALUES ('partials', 0);
	CREATE TRIGGER usage_blocks_i AFTER INSERT ON blocks BEGIN
		UPDATE usage SET bytes = bytes + length(new.data) WHERE class = 'blocks'; END;
	CREATE TRIGGER usage_blocks_d AFTER DELETE ON blocks BEGIN
		UPDATE usage SET bytes = bytes - length(old.data) WHERE class = 'blocks'; END;
	CREATE TRIGGER usage_blobs_i AFTER INSERT ON blobs BEGIN
		UPDATE usage SET bytes = bytes + new.size WHERE class = CASE WHEN new.src_path IS NULL THEN 'blobs' ELSE 'nocopy' END;
		UPDATE usage SET bytes = bytes + new.outboard * (8 + 64 * ((new.size + 16383) / 16384 - 1)) WHERE class = 'outboards'; END;
	CREATE TRIGGER usage_blobs_d AFTER DELETE ON blobs BEGIN
		UPDATE usage SET bytes = bytes - old.size WHERE class = CASE WHEN old.src_path IS NULL THEN 'blobs' ELSE 'nocopy' END;
		UPDATE usage SET bytes = bytes - old.outboard * (8 + 64 * ((old.size + 16383) / 16384 - 1)) WHERE class = 'outboards'; END;
	CREATE TRIGGER usage_partials_i AFTER INSERT ON partials BEGIN
		UPDATE usage SET bytes = bytes + new.held WHERE class = 'partials';
		UPDATE usage SET bytes = bytes + 8 + 64 * ((new.size + 16383) / 16384 - 1) WHERE class = 'outboards'; END;
	CREATE TRIGGER usage_partials_d AFTER DELETE ON partials BEGIN
		UPDATE usage SET bytes = bytes - old.held WHERE class = 'partials';
		UPDATE usage SET bytes = bytes - 8 - 64 * ((old.size + 16383) / 16384 - 1) WHERE class = 'outboards'; END;
	CREATE TRIGGER usage_partials_u AFTER UPDATE OF held ON partials BEGIN
		UPDATE usage SET bytes = bytes + new.held - old.held WHERE class = 'partials'; END;`,
		post: migrateHeld},
	// 4: the second codec a block was stored under (0 when none). A block row is keyed by
	// multihash; the same bytes may be a raw blob and a DRISL node, and a DRISL read is
	// served only from bytes stored as DRISL (0077: a raw file's bytes read as DRISL would
	// be a node with links nobody published).
	{sql: `ALTER TABLE blocks ADD COLUMN alt INTEGER NOT NULL DEFAULT 0;`},
	// 5: blocks as a rowid table with a separate multihash index. Its rows carry inline
	// blobs of up to 16 KiB; WITHOUT ROWID keeps whole rows in the interior b-tree pages,
	// so every lookup and insert read overflow pages on the way down (2-4x slower). The
	// copy keeps the usage counters (DROP TABLE fires no triggers).
	{sql: `CREATE TABLE blocks_new (
		mh BLOB NOT NULL PRIMARY KEY, codec INTEGER NOT NULL, data BLOB NOT NULL,
		created INTEGER NOT NULL, alt INTEGER NOT NULL DEFAULT 0
	);
	INSERT INTO blocks_new (mh, codec, data, created, alt) SELECT mh, codec, data, created, alt FROM blocks;
	DROP TABLE blocks;
	ALTER TABLE blocks_new RENAME TO blocks;
	CREATE TRIGGER usage_blocks_i AFTER INSERT ON blocks BEGIN
		UPDATE usage SET bytes = bytes + length(new.data) WHERE class = 'blocks'; END;
	CREATE TRIGGER usage_blocks_d AFTER DELETE ON blocks BEGIN
		UPDATE usage SET bytes = bytes - length(old.data) WHERE class = 'blocks'; END;`},
}

// migrateHeld fills partials.held from the group bitmaps (migration 3).
func migrateHeld(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT mh, size, groups FROM partials`)
	if err != nil {
		return err
	}
	type row struct {
		mh   []byte
		held int64
	}
	var upd []row
	for rows.Next() {
		var mh, groups []byte
		var size int64
		if err := rows.Scan(&mh, &size, &groups); err != nil {
			rows.Close()
			return err
		}
		upd = append(upd, row{mh, heldBytes(groups, uint64(size))})
	}
	rows.Close()
	for _, r := range upd {
		if _, err := tx.Exec(`UPDATE partials SET held = ? WHERE mh = ?`, r.held, r.mh); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion is the schema version this binary writes.
var SchemaVersion = len(migrations)

// ErrNewerSchema is returned by Open for a database written by a newer build. Nothing is
// changed: downgrades are not supported (docs/operations.md "Upgrades and compatibility").
var ErrNewerSchema = errors.New("store: database schema is newer than this build supports")

// migrateHook, when set (tests), runs inside each migration's transaction before it
// commits, with the version being reached.
var migrateHook func(version int)

// migrate applies the forward migrations, each in its own transaction together with the
// user_version bump, so an interrupted upgrade leaves the database at the last completed
// version and the next Open resumes from there.
func migrate(db *sql.DB) error { return migrateTo(db, len(migrations)) }

func migrateTo(db *sql.DB, target int) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("%w (database %d, this build %d)", ErrNewerSchema, v, len(migrations))
	}
	if v < target {
		// Migrations rewrite tables (5 copies blocks): return the pages they freed and
		// the WAL they grew, which count against the store's budget (best effort).
		defer func() {
			_, _ = db.Exec(`PRAGMA incremental_vacuum`)
			_, _ = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		}()
	}
	for ; v < target; v++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v].sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", v+1, err)
		}
		if post := migrations[v].post; post != nil {
			if err := post(tx); err != nil {
				tx.Rollback()
				return fmt.Errorf("store: migration %d: %w", v+1, err)
			}
		}
		if migrateHook != nil {
			migrateHook(v + 1)
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
	// Physical is the bytes charged for the object (A1): data plus outboard for a copied
	// blob, the outboard alone for a --nocopy reference, verified bytes plus outboard
	// for a partial blob.
	Physical uint64
}

// Has reports whether the store holds the object with c's multihash. A raw CID is held
// by multihash (any stored bytes read as raw bytes); a DRISL CID is held only when the
// bytes were stored as DRISL (codecMatch). A --nocopy blob whose source changed is
// re-verified and, if it no longer matches, reported absent.
func (s *Store) Has(c cid.CID) (Info, bool, error) {
	m := c.Multihash()
	var codec, alt int
	var size int64
	err := s.queryRow(`SELECT codec, alt, length(data) FROM blocks WHERE mh = ?`, mhKey(m)).Scan(&codec, &alt, &size)
	if err == nil {
		if codecMatch(c.Codec(), codec, alt) {
			if alt == int(c.Codec()) {
				codec = alt
			}
			return Info{Codec: cid.Codec(codec), Size: uint64(size), Complete: true, Physical: uint64(size)}, true, nil
		}
		return Info{}, false, nil // raw bytes asked for as DRISL; blobs and partials are raw only
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Info{}, false, err
	}
	if c.Codec() != cid.Raw {
		return Info{}, false, nil // the blob tables hold raw bytes only
	}
	row, err := s.blobRow(m)
	if err == nil {
		if row.srcPath != "" {
			if ok, err := s.checkNoCopy(row); err != nil || !ok {
				return Info{}, false, err
			}
		}
		info := Info{Codec: cid.Raw, Size: row.size, Complete: true, NoCopy: row.srcPath != ""}
		if !info.NoCopy {
			info.Physical = row.size
		}
		if row.outboard {
			info.Physical += blob.OutboardSize(row.size)
		}
		return info, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Info{}, false, err
	}
	var held int64
	err = s.queryRow(`SELECT size, held FROM partials WHERE mh = ?`, mhKey(m)).Scan(&size, &held)
	if err == nil {
		return Info{Codec: cid.Raw, Size: uint64(size), Physical: uint64(held) + blob.OutboardSize(uint64(size))}, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Info{}, false, nil
	}
	return Info{}, false, err
}

// Get returns a block held in the database: DRISL structure or a small inline blob. It
// implements mst.Store together with Put. A DRISL CID whose bytes were stored only as a
// raw blob is ErrNotFound (codecMatch): content-addressed bytes are the same, but their
// interpretation as a node with links was never vouched for (0077).
func (s *Store) Get(c cid.CID) ([]byte, error) {
	var data []byte
	var codec, alt int
	err := s.queryRow(`SELECT data, codec, alt FROM blocks WHERE mh = ?`, mhKey(c.Multihash())).Scan(&data, &codec, &alt)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !codecMatch(c.Codec(), codec, alt) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, c)
	}
	return data, err
}

// codecMatch reports whether a block row stored under codec (and alt, 0 for none) may be
// read as want: raw reads any bytes; DRISL needs the bytes stored as DRISL.
func codecMatch(want cid.Codec, codec, alt int) bool {
	return want == cid.Raw || codec == int(want) || alt == int(want)
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

// StateStore is the store as signed cluster state writes it (D17 A5): Put may use the
// state reserve that content admission leaves free, so an authority with a full store
// can still write member and head records.
type StateStore struct{ s *Store }

// State returns the store's view for signed cluster-state writes.
func (s *Store) State() StateStore { return StateStore{s} }

// Get is Store.Get.
func (w StateStore) Get(c cid.CID) ([]byte, error) { return w.s.Get(c) }

// Put is Store.Put admitted against the full headroom, the state reserve included.
func (w StateStore) Put(c cid.CID, data []byte) error {
	if c.Codec() == cid.Raw && len(data) > inlineMax {
		return w.s.Put(c, data) // state is DRISL; a large raw blob is content
	}
	if err := c.Verify(data); err != nil {
		return err
	}
	return w.s.putBlockAs(c.Codec(), c.Multihash(), data, true)
}

func (s *Store) putBlock(codec cid.Codec, m cid.Multihash, data []byte) error {
	return s.putBlockAs(codec, m, data, false)
}

func (s *Store) putBlockAs(codec cid.Codec, m cid.Multihash, data []byte, state bool) error {
	if err := s.admitAs(int64(len(data)), state); err != nil {
		return err
	}
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	_, err := s.exec(insertBlock, mhKey(m), int(codec), data, s.nowUnix())
	return err
}

// insertBlock stores a block row. The same bytes stored under the other codec record it
// as alt (Get, codecMatch).
const insertBlock = `INSERT INTO blocks (mh, codec, data, created) VALUES (?, ?, ?, ?)
	ON CONFLICT (mh) DO UPDATE SET alt = excluded.codec WHERE blocks.codec != excluded.codec AND blocks.alt = 0`

// Block is one whole object for PutMany.
type Block struct {
	CID  cid.CID
	Data []byte
}

// PutMany is Put for many objects. It verifies all of them first and stores the
// database-held ones in one transaction, which costs about as much as one Put: a caller
// storing many small objects (a /nu/tree/1 response) should prefer it. On error none of
// the database-held objects are stored.
func (s *Store) PutMany(blocks []Block) error {
	var need int64
	for _, b := range blocks {
		if err := b.CID.Verify(b.Data); err != nil {
			return err
		}
		if b.CID.Codec() != cid.Raw || len(b.Data) <= inlineMax {
			need += int64(len(b.Data))
		}
	}
	if err := s.admit(need); err != nil {
		return err
	}
	if err := s.insertBlocks(func(add func(cid.Codec, cid.Multihash, []byte) error) error {
		for _, b := range blocks {
			if b.CID.Codec() != cid.Raw || len(b.Data) <= inlineMax {
				if err := add(b.CID.Codec(), b.CID.Multihash(), b.Data); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, b := range blocks {
		if b.CID.Codec() == cid.Raw && len(b.Data) > inlineMax {
			if _, err := s.PutBlob(b.CID, bytesReader(b.Data), int64(len(b.Data))); err != nil {
				return err
			}
		}
	}
	return nil
}

// insertBlocks runs fill in one transaction, giving it an add that inserts a block row
// with one prepared statement. Admission is the caller's.
func (s *Store) insertBlocks(fill func(add func(cid.Codec, cid.Multihash, []byte) error) error) error {
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
	if err := fill(func(codec cid.Codec, m cid.Multihash, data []byte) error {
		_, err := ptx.Exec(insertBlock, mhKey(m), int(codec), data, now)
		return err
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// preparedTx runs a transaction's statements prepared once per query text, so a batch of
// many rows does not parse the same SQL for each one.
type preparedTx struct {
	tx    *sql.Tx
	stmts map[string]*sql.Stmt
}

func newPreparedTx(tx *sql.Tx) *preparedTx { return &preparedTx{tx: tx, stmts: map[string]*sql.Stmt{}} }

func (p *preparedTx) stmt(q string) (*sql.Stmt, error) {
	if st := p.stmts[q]; st != nil {
		return st, nil
	}
	st, err := p.tx.Prepare(q)
	if err != nil {
		return nil, err
	}
	p.stmts[q] = st
	return st, nil
}

func (p *preparedTx) Exec(q string, args ...any) (sql.Result, error) {
	st, err := p.stmt(q)
	if err != nil {
		return nil, err
	}
	return st.Exec(args...)
}

// QueryRow is sql.Tx.QueryRow; a prepare error is reported by Scan.
func (p *preparedTx) QueryRow(q string, args ...any) *sql.Row {
	st, err := p.stmt(q)
	if err != nil {
		return p.tx.QueryRow(q, args...)
	}
	return st.QueryRow(args...)
}

func (p *preparedTx) close() {
	for _, st := range p.stmts {
		st.Close()
	}
}
