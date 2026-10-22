package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
)

// Pin modes.
const (
	Recursive = "recursive"
	Direct    = "direct"
)

// Pin sources (D17).
const (
	SourceExplicit           = "explicit"
	SourceFollow             = "follow"
	SourceMyNames            = "my-names"
	SourceCluster            = "cluster"
	SourceClusterSurplus     = "cluster-surplus"
	SourceCollectionSelect   = "collection-select"
	SourceCollectionManual   = "collection-manual"
	SourceCollectionConflict = "collection-conflict"
	// SourceCollectionPush protects the metadata and files of an item push until the
	// item is in the writer head (or the push failed).
	SourceCollectionPush = "collection-push"
	// SourcePublic marks a root the operator chose to serve on the public HTTPS listener
	// (operations.md P2); it protects the root like an explicit pin.
	SourcePublic = "public"
)

var sources = map[string]bool{
	SourceExplicit: true, SourceFollow: true, SourceMyNames: true, SourceCluster: true,
	SourceClusterSurplus: true, SourceCollectionSelect: true, SourceCollectionManual: true,
	SourceCollectionConflict: true, SourceCollectionPush: true, SourcePublic: true,
}

// Pin protects an object, and for recursive pins everything its walk profile reaches, from
// GC. The same CID may be pinned under several sources/names; each is released separately.
type Pin struct {
	CID     cid.CID
	Mode    string // Recursive or Direct
	Profile string // walk profile for Recursive pins, e.g. "nu-tree"
	Source  string
	Name    string // optional context (followed name, dataset, collection), part of the key
}

// Pin adds a pin (idempotent).
func (s *Store) Pin(p Pin) error {
	if !p.CID.Defined() || !sources[p.Source] {
		return fmt.Errorf("store: invalid pin (cid %v, source %q)", p.CID, p.Source)
	}
	switch p.Mode {
	case Direct:
		p.Profile = "direct"
	case Recursive:
		if _, ok := s.profile(p.Profile); !ok {
			return fmt.Errorf("store: unknown walk profile %q", p.Profile)
		}
	default:
		return fmt.Errorf("store: invalid pin mode %q", p.Mode)
	}
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	_, err := s.db.Exec(`INSERT INTO pins (cid, mode, profile, source, name, created) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT DO UPDATE SET profile = excluded.profile`,
		p.CID.Bytes(), p.Mode, p.Profile, p.Source, p.Name, s.nowUnix())
	return err
}

// Unpin removes one pin and reports whether it existed.
func (s *Store) Unpin(p Pin) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM pins WHERE cid = ? AND mode = ? AND source = ? AND name = ?`,
		p.CID.Bytes(), p.Mode, p.Source, p.Name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Pins lists every pin.
func (s *Store) Pins() ([]Pin, error) {
	rows, err := s.db.Query(`SELECT cid, mode, profile, source, name FROM pins ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pin
	for rows.Next() {
		var b []byte
		var p Pin
		if err := rows.Scan(&b, &p.Mode, &p.Profile, &p.Source, &p.Name); err != nil {
			return nil, err
		}
		if p.CID, err = cid.Decode(b); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// WalkStats summarises a walk from a pin.
type WalkStats struct {
	Objects, Missing int
	Bytes            uint64 // logical bytes of the present objects
}

// Complete reports whether every reachable object is present.
func (w WalkStats) Complete() bool { return w.Missing == 0 }

// Walk visits every object reachable from p under its walk profile, once each, calling fn
// (if non-nil) with each CID and whether it is held complete. Structure blocks that are
// missing end the walk below them.
func (s *Store) Walk(ctx context.Context, p Pin, fn func(c cid.CID, held bool) error) (WalkStats, error) {
	var f func(c cid.CID, info Info, held bool) error
	if fn != nil {
		f = func(c cid.CID, _ Info, held bool) error { return fn(c, held) }
	}
	return s.walk(ctx, p, f)
}

// walk is Walk with the object's Info (zero when absent).
func (s *Store) walk(ctx context.Context, p Pin, fn func(c cid.CID, info Info, held bool) error) (WalkStats, error) {
	var links LinksFunc
	if p.Mode == Recursive {
		var ok bool
		if links, ok = s.profile(p.Profile); !ok {
			return WalkStats{}, fmt.Errorf("store: unknown walk profile %q", p.Profile)
		}
	}
	var st WalkStats
	pre, err := s.prefetch(ctx, p.CID, links)
	if err != nil {
		return st, err
	}
	seen := map[cid.Multihash]bool{}
	stack := []cid.CID{p.CID}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[c.Multihash()] {
			continue
		}
		seen[c.Multihash()] = true
		st.Objects++
		e, cached := pre.objs[c.Multihash()]
		info, ok := e.info, e.ok
		if !cached {
			if info, ok, err = s.Has(c); err != nil {
				return st, err
			}
		}
		held := ok && info.Complete
		if held {
			st.Bytes += info.Size
		} else {
			st.Missing++
		}
		if fn != nil {
			if err := fn(c, info, held); err != nil {
				return st, err
			}
		}
		if links == nil || !held || c.Codec() != cid.DRISL {
			continue
		}
		data := e.data
		if data == nil || e.codec != c.Codec() {
			if data, err = s.Get(c); err != nil {
				return st, err
			}
		}
		kids, err := links(c, data)
		if err != nil {
			return st, fmt.Errorf("store: walking %s: %w", c, err)
		}
		for i := len(kids) - 1; i >= 0; i-- { // keep profile order
			stack = append(stack, kids[i])
		}
	}
	return st, nil
}

// GCOptions configures a collection.
type GCOptions struct {
	// Grace keeps unpinned objects written (or partials updated) more recently than this,
	// so in-progress adds and fetches survive until they are pinned.
	Grace time.Duration
}

// GCResult reports what a collection removed.
type GCResult struct {
	Blocks, Blobs, Partials, Orphans int
}

// GC marks everything reachable from pins and removes the rest. Rows are deleted in one
// transaction before their files, so a crash leaves at most orphan files, which the next GC
// removes. --nocopy sources are never deleted.
func (s *Store) GC(ctx context.Context, o GCOptions) (GCResult, error) {
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	pins, err := s.Pins()
	if err != nil {
		return GCResult{}, err
	}
	live := map[cid.Multihash]bool{}
	for _, p := range pins {
		if _, err := s.Walk(ctx, p, func(c cid.CID, _ bool) error {
			live[c.Multihash()] = true
			return nil
		}); err != nil {
			return GCResult{}, err
		}
	}
	cutoff := s.now().Add(-o.Grace).UnixNano()
	var res GCResult
	var files []string
	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	for _, t := range []struct {
		table, when string
		n           *int
	}{{"blocks", "created", &res.Blocks}, {"blobs", "created", &res.Blobs}, {"partials", "updated", &res.Partials}} {
		dead, err := deadKeys(tx, t.table, t.when, cutoff, live)
		if err != nil {
			return res, err
		}
		for _, k := range dead {
			if _, err := tx.Exec(`DELETE FROM `+t.table+` WHERE mh = ?`, k); err != nil {
				return res, err
			}
			m, _ := cid.DecodeMultihash(k)
			switch t.table {
			case "blobs":
				files = append(files, s.blobPath(m), s.blobPath(m)+".obao")
			case "partials":
				files = append(files, s.partialPath(m), s.partialPath(m)+".obao")
			}
		}
		*t.n = len(dead)
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	if res.Blocks+res.Blobs > 0 {
		s.removals.Add(1)
	}
	if res.Blocks+res.Blobs+res.Partials > 0 {
		_ = s.reclaimDB() // best effort: the rows are gone either way
	}
	for _, f := range files {
		_ = os.Remove(f) // a nocopy blob's data path is its source, never under blobs/
	}
	res.Orphans, err = s.sweepOrphans(cutoff)
	return res, err
}

func deadKeys(tx *sql.Tx, table, when string, cutoff int64, live map[cid.Multihash]bool) ([][]byte, error) {
	rows, err := tx.Query(`SELECT mh FROM `+table+` WHERE `+when+` <= ?`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var dead [][]byte
	for rows.Next() {
		var k []byte
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		m, err := cid.DecodeMultihash(k)
		if err != nil || !live[m] {
			dead = append(dead, k)
		}
	}
	return dead, rows.Err()
}

// sweepOrphans removes files under blobs/ and partial/ that have no row (left by a crash
// between rename and commit, or between GC commit and file removal).
func (s *Store) sweepOrphans(cutoff int64) (int, error) {
	n := 0
	for _, sub := range []struct{ dir, table string }{{"blobs", "blobs"}, {"partial", "partials"}} {
		root := filepath.Join(s.dir, sub.dir)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			name := strings.TrimSuffix(d.Name(), ".obao")
			k, err := hex.DecodeString(name)
			if err != nil {
				return nil
			}
			fi, err := d.Info()
			if err != nil || fi.ModTime().UnixNano() > cutoff {
				return nil
			}
			var one int
			err = s.db.QueryRow(`SELECT 1 FROM `+sub.table+` WHERE mh = ?`, k).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				if os.Remove(path) == nil {
					n++
				}
				return nil
			}
			return err
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// GCIfAbove runs GC when Usage exceeds high bytes; GCIfNeeded applies the configured
// limits instead.
func (s *Store) GCIfAbove(ctx context.Context, high int64, o GCOptions) (bool, GCResult, error) {
	u, err := s.Usage()
	if err != nil || u <= high {
		return false, GCResult{}, err
	}
	r, err := s.GC(ctx, o)
	return true, r, err
}
