package store

import (
	"context"
	"database/sql"
	"strings"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
)

// Batched lookups for completeness walks (0078). A walk over a large tree used to issue
// one Has (two to three SQL queries) and one Get per object, so readiness evaluation of a
// 2,000-file tree cost seconds per pass; these answer a whole tree level in a few queries.

// manyBatch bounds the parameters of one IN (...) query (SQLite's default limit is 32766).
const manyBatch = 500

// Removals is a counter that changes after every committed removal of held objects (GC,
// release of corrupt objects, an invalidated --nocopy source). A caller may cache "held"
// facts and trust them while Removals is unchanged; additions never decrease what is held.
// Read it before the lookups whose results are cached.
func (s *Store) Removals() uint64 { return s.removals.Load() }

// HasMany is Has for many objects: the result maps every requested CID that Has would
// report held (complete or partial) to the Info Has returns for it; absent objects are
// missing from it. Like Has it is codec-aware per CID (0077): a raw CID is held by
// multihash, a DRISL CID only when its bytes were stored as DRISL (codec or alt), so the
// same multihash may be held as raw and not as DRISL. --nocopy sources are re-verified.
func (s *Store) HasMany(cs []cid.CID) (map[cid.CID]Info, error) {
	// What the store holds per multihash: a blocks row (codec, alt) or a raw blob/partial.
	type row struct {
		block      bool
		codec, alt int
		info       Info
	}
	rows := make(map[cid.Multihash]row, len(cs))
	var keys []any
	rawWanted := map[cid.Multihash]bool{}
	seen := make(map[cid.Multihash]bool, len(cs))
	for _, c := range cs {
		m := c.Multihash()
		if c.Codec() == cid.Raw {
			rawWanted[m] = true
		}
		if !seen[m] {
			seen[m] = true
			keys = append(keys, mhKey(m))
		}
	}
	var nocopy []blobRow
	pending := map[cid.Multihash]bool{} // --nocopy rows awaiting verification
	for len(keys) > 0 {
		batch := keys[:min(len(keys), manyBatch)]
		keys = keys[len(batch):]
		if err := s.scanMany(`SELECT mh, codec, alt, length(data) FROM blocks WHERE mh IN (`+placeholders(len(batch))+`)`, batch, func(m cid.Multihash, r *sql.Rows) error {
			var codec, alt int
			var size int64
			if err := r.Scan(new([]byte), &codec, &alt, &size); err != nil {
				return err
			}
			rows[m] = row{block: true, codec: codec, alt: alt, info: Info{Size: uint64(size), Complete: true, Physical: uint64(size)}}
			return nil
		}); err != nil {
			return nil, err
		}
		// The blob tables hold raw bytes only: look there for raw requests not in blocks.
		var rest []any
		for _, k := range batch {
			if m, err := cid.DecodeMultihash(k.([]byte)); err == nil && rawWanted[m] {
				if _, ok := rows[m]; !ok {
					rest = append(rest, k)
				}
			}
		}
		if len(rest) == 0 {
			continue
		}
		if err := s.scanMany(`SELECT mh, size, outboard, src_path, src_mtime FROM blobs WHERE mh IN (`+placeholders(len(rest))+`)`, rest, func(m cid.Multihash, r *sql.Rows) error {
			br := blobRow{m: m}
			var size int64
			var src sql.NullString
			var mtime sql.NullInt64
			if err := r.Scan(new([]byte), &size, &br.outboard, &src, &mtime); err != nil {
				return err
			}
			br.size, br.srcPath, br.srcMtime = uint64(size), src.String, mtime.Int64
			if br.srcPath != "" {
				nocopy = append(nocopy, br) // verified below, outside the query
				pending[m] = true
				return nil
			}
			info := Info{Codec: cid.Raw, Size: br.size, Complete: true, Physical: br.size}
			if br.outboard {
				info.Physical += blob.OutboardSize(br.size)
			}
			rows[m] = row{info: info}
			return nil
		}); err != nil {
			return nil, err
		}
		var parts []any
		for _, k := range rest {
			m, _ := cid.DecodeMultihash(k.([]byte))
			if _, ok := rows[m]; !ok && !pending[m] {
				parts = append(parts, k)
			}
		}
		if len(parts) == 0 {
			continue
		}
		if err := s.scanMany(`SELECT mh, size, held FROM partials WHERE mh IN (`+placeholders(len(parts))+`)`, parts, func(m cid.Multihash, r *sql.Rows) error {
			var size, held int64
			if err := r.Scan(new([]byte), &size, &held); err != nil {
				return err
			}
			rows[m] = row{info: Info{Codec: cid.Raw, Size: uint64(size), Physical: uint64(held) + blob.OutboardSize(uint64(size))}}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for _, r := range nocopy {
		ok, err := s.checkNoCopy(r)
		if err != nil {
			return nil, err
		}
		if ok {
			info := Info{Codec: cid.Raw, Size: r.size, Complete: true, NoCopy: true}
			if r.outboard {
				info.Physical = blob.OutboardSize(r.size)
			}
			rows[r.m] = row{info: info}
		}
	}
	out := make(map[cid.CID]Info, len(cs))
	for _, c := range cs {
		r, ok := rows[c.Multihash()]
		switch {
		case !ok:
		case r.block:
			if !codecMatch(c.Codec(), r.codec, r.alt) {
				continue // raw bytes asked for as DRISL
			}
			info := r.info
			info.Codec = cid.Codec(r.codec)
			if r.alt == int(c.Codec()) {
				info.Codec = c.Codec()
			}
			out[c] = info
		case c.Codec() == cid.Raw:
			out[c] = r.info
		}
	}
	return out, nil
}

// GetMany is Get for many blocks: the result maps every requested CID that Get would
// answer (structure or an inline blob, codec-checked per CID like Get) to its bytes.
func (s *Store) GetMany(cs []cid.CID) (map[cid.CID][]byte, error) {
	type row struct {
		codec, alt int
		data       []byte
	}
	rows := make(map[cid.Multihash]row, len(cs))
	keys := make([]any, 0, len(cs))
	for _, c := range cs {
		keys = append(keys, mhKey(c.Multihash()))
	}
	for len(keys) > 0 {
		batch := keys[:min(len(keys), manyBatch)]
		keys = keys[len(batch):]
		if err := s.scanMany(`SELECT mh, codec, alt, data FROM blocks WHERE mh IN (`+placeholders(len(batch))+`)`, batch, func(m cid.Multihash, r *sql.Rows) error {
			var w row
			if err := r.Scan(new([]byte), &w.codec, &w.alt, &w.data); err != nil {
				return err
			}
			rows[m] = w
			return nil
		}); err != nil {
			return nil, err
		}
	}
	out := make(map[cid.CID][]byte, len(cs))
	for _, c := range cs {
		if r, ok := rows[c.Multihash()]; ok && codecMatch(c.Codec(), r.codec, r.alt) {
			out[c] = r.data
		}
	}
	return out, nil
}

// scanMany runs q, whose first column is the multihash key, and calls fn per row with the
// decoded key; fn scans the row itself (its first destination receives the key again).
func (s *Store) scanMany(q string, args []any, fn func(cid.Multihash, *sql.Rows) error) error {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key []byte
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		dest := make([]any, len(cols))
		dest[0] = &key
		for i := 1; i < len(cols); i++ {
			dest[i] = new(any)
		}
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		m, err := cid.DecodeMultihash(key)
		if err != nil {
			continue
		}
		if err := fn(m, rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// prefetchMax bounds the objects a walk looks up in batches before it visits them; past
// it the walk falls back to one lookup per object.
const prefetchMax = 1 << 17

type prefetched struct {
	info Info
	ok   bool
	data []byte // structure block bytes (nil for blobs and missing objects)
}

// prefetchSet holds lookups by full CID: the same multihash requested as raw and as DRISL
// are separate answers (0077).
type prefetchSet struct{ objs map[cid.CID]prefetched }

// prefetch looks up root's reachable objects level by level (HasMany, then GetMany for
// held DRISL structure under links), so a depth-first walk can visit them from memory in
// its own order. links nil: only root.
func (s *Store) prefetch(ctx context.Context, root cid.CID, links LinksFunc) (prefetchSet, error) {
	ps := prefetchSet{objs: map[cid.CID]prefetched{}}
	frontier := []cid.CID{root}
	for len(frontier) > 0 && len(ps.objs) < prefetchMax {
		if err := ctx.Err(); err != nil {
			return ps, err
		}
		var level []cid.CID
		for _, c := range frontier {
			if _, ok := ps.objs[c]; !ok {
				ps.objs[c] = prefetched{} // dedupes the level; set below
				level = append(level, c)
			}
		}
		has, err := s.HasMany(level)
		if err != nil {
			return ps, err
		}
		var structs []cid.CID
		for _, c := range level {
			info, ok := has[c]
			ps.objs[c] = prefetched{info: info, ok: ok}
			if links != nil && ok && info.Complete && c.Codec() == cid.DRISL {
				structs = append(structs, c)
			}
		}
		blocks, err := s.GetMany(structs)
		if err != nil {
			return ps, err
		}
		frontier = nil
		for _, c := range structs {
			data, ok := blocks[c]
			if !ok {
				continue // the walk reads it itself
			}
			e := ps.objs[c]
			e.data = data
			ps.objs[c] = e
			kids, err := links(c, data)
			if err != nil {
				continue // the walk reports it in order
			}
			frontier = append(frontier, kids...)
		}
	}
	return ps, nil
}
