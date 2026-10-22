package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/whyrusleeping/nu/core/cid"
)

// SQLite space reclaim (0078). Rows deleted by GC used to leave free pages inside nu.db
// (and a large WAL file behind), so the physical bytes a release was estimated to free
// never left the store. The database now uses auto_vacuum=INCREMENTAL: GC returns the
// free pages to the filesystem (incremental_vacuum) and truncates the WAL.

// autoVacuumIncremental is SQLite's PRAGMA auto_vacuum value for INCREMENTAL.
const autoVacuumIncremental = 2

// enableIncrementalVacuum switches db to auto_vacuum=INCREMENTAL. A database created
// before 0078 is rewritten once (VACUUM, needing free space about the size of nu.db); a
// failed conversion leaves it as it was, and reclaim then only truncates the WAL.
func enableIncrementalVacuum(db *sql.DB) error {
	var mode int
	if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return err
	}
	if mode == autoVacuumIncremental {
		return nil
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA auto_vacuum = %d`, autoVacuumIncremental)); err != nil {
		return err
	}
	// The pragma alone only takes effect before the database is initialized (the WAL
	// journal mode already wrote its header); otherwise a VACUUM applies it.
	if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil || mode == autoVacuumIncremental {
		return err
	}
	_, _ = db.Exec(`VACUUM`) // best effort (for example short of disk space)
	return nil
}

// reclaimDB returns the database's free pages to the filesystem and truncates the WAL
// after a collection removed rows.
func (s *Store) reclaimDB() error {
	if _, err := s.db.Exec(`PRAGMA incremental_vacuum`); err != nil {
		return err
	}
	_, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// DBReclaimable is the database space a GC returns to the filesystem besides the rows it
// deletes: free pages inside nu.db and the WAL file.
func (s *Store) DBReclaimable() (int64, error) {
	var pages, size int64
	if err := s.db.QueryRow(`PRAGMA freelist_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := s.db.QueryRow(`PRAGMA page_size`).Scan(&size); err != nil {
		return 0, err
	}
	n := pages * size
	if fi, err := os.Stat(filepath.Join(s.dir, "nu.db-wal")); err == nil {
		n += fi.Size()
	}
	return n, nil
}

// Reclaimer returns, for an object held as info, the bytes deleting it returns to the
// filesystem once GC runs: a blob file with its outboard, or for a database block (DRISL
// structure, inline blob) the overflow pages of its row. A row's in-page part becomes
// free space inside a page that keeps other rows; SQLite reuses it but does not return it.
func (s *Store) Reclaimer() (func(c cid.CID, info Info) int64, error) {
	var page int64
	if err := s.db.QueryRow(`PRAGMA page_size`).Scan(&page); err != nil {
		return nil, err
	}
	// Index b-tree (WITHOUT ROWID) payload limits, as in SQLite's btree.c.
	usable := page
	maxLocal := (usable-12)*64/255 - 23
	minLocal := (usable-12)*32/255 - 23
	return func(c cid.CID, info Info) int64 {
		inDB := c.Codec() != cid.Raw || (info.Size <= inlineMax && !info.NoCopy)
		if !inDB {
			return int64(info.Physical)
		}
		// Record: header, key (multihash) and codec/data/created columns.
		payload := int64(info.Size) + int64(len(c.Multihash().Bytes())) + 24
		if payload <= maxLocal {
			return 0
		}
		local := minLocal + (payload-minLocal)%(usable-4)
		if local > maxLocal {
			local = minLocal
		}
		over := (payload - local + usable - 5) / (usable - 4)
		return over * page
	}, nil
}
