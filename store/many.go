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

// HasMany is Has for many objects: the result maps the multihash of every object held
// (complete or partial, as Has reports it) to its Info; absent objects are missing from it.
// --nocopy sources are re-verified as Has does.
func (s *Store) HasMany(cs []cid.CID) (map[cid.Multihash]Info, error) {
	out := make(map[cid.Multihash]Info, len(cs))
	keys := make([]any, 0, len(cs))
	seen := make(map[cid.Multihash]bool, len(cs))
	for _, c := range cs {
		m := c.Multihash()
		if !seen[m] {
			seen[m] = true
			keys = append(keys, mhKey(m))
		}
	}
	var nocopy []blobRow
	pending := map[cid.Multihash]bool{}
	for len(keys) > 0 {
		batch := keys[:min(len(keys), manyBatch)]
		keys = keys[len(batch):]
		in := placeholders(len(batch))
		if err := s.scanMany(`SELECT mh, codec, length(data) FROM blocks WHERE mh IN (`+in+`)`, batch, func(m cid.Multihash, rows *sql.Rows) error {
			var codec int
			var size int64
			if err := rows.Scan(new([]byte), &codec, &size); err != nil {
				return err
			}
			out[m] = Info{Codec: cid.Codec(codec), Size: uint64(size), Complete: true, Physical: uint64(size)}
			return nil
		}); err != nil {
			return nil, err
		}
		rest := remaining(batch, out, nil)
		if len(rest) == 0 {
			continue
		}
		in = placeholders(len(rest))
		if err := s.scanMany(`SELECT mh, size, outboard, src_path, src_mtime FROM blobs WHERE mh IN (`+in+`)`, rest, func(m cid.Multihash, rows *sql.Rows) error {
			r := blobRow{m: m}
			var size int64
			var src sql.NullString
			var mtime sql.NullInt64
			if err := rows.Scan(new([]byte), &size, &r.outboard, &src, &mtime); err != nil {
				return err
			}
			r.size, r.srcPath, r.srcMtime = uint64(size), src.String, mtime.Int64
			if r.srcPath != "" {
				nocopy = append(nocopy, r) // verified below, outside the query
				pending[m] = true
				return nil
			}
			info := Info{Codec: cid.Raw, Size: r.size, Complete: true, Physical: r.size}
			if r.outboard {
				info.Physical += blob.OutboardSize(r.size)
			}
			out[m] = info
			return nil
		}); err != nil {
			return nil, err
		}
		rest = remaining(rest, out, pending)
		if len(rest) == 0 {
			continue
		}
		in = placeholders(len(rest))
		if err := s.scanMany(`SELECT mh, size, held FROM partials WHERE mh IN (`+in+`)`, rest, func(m cid.Multihash, rows *sql.Rows) error {
			var size, held int64
			if err := rows.Scan(new([]byte), &size, &held); err != nil {
				return err
			}
			out[m] = Info{Codec: cid.Raw, Size: uint64(size), Physical: uint64(held) + blob.OutboardSize(uint64(size))}
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
			out[r.m] = info
		}
	}
	return out, nil
}

// Block is a database block with the codec it was first stored under.
type Block struct {
	Codec cid.Codec
	Data  []byte
}

// GetMany is Get for many blocks: the result maps the multihash of every block held in
// the database (structure or an inline blob) to its codec and bytes. Callers that decode
// a block by the codec of the CID they asked for should compare it with Block.Codec.
func (s *Store) GetMany(cs []cid.CID) (map[cid.Multihash]Block, error) {
	out := make(map[cid.Multihash]Block, len(cs))
	keys := make([]any, 0, len(cs))
	for _, c := range cs {
		keys = append(keys, mhKey(c.Multihash()))
	}
	for len(keys) > 0 {
		batch := keys[:min(len(keys), manyBatch)]
		keys = keys[len(batch):]
		if err := s.scanMany(`SELECT mh, codec, data FROM blocks WHERE mh IN (`+placeholders(len(batch))+`)`, batch, func(m cid.Multihash, rows *sql.Rows) error {
			var b Block
			var codec int
			if err := rows.Scan(new([]byte), &codec, &b.Data); err != nil {
				return err
			}
			b.Codec = cid.Codec(codec)
			out[m] = b
			return nil
		}); err != nil {
			return nil, err
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

// remaining returns the keys of batch neither in found nor in skip.
func remaining(batch []any, found map[cid.Multihash]Info, skip map[cid.Multihash]bool) []any {
	var out []any
	for _, k := range batch {
		m, err := cid.DecodeMultihash(k.([]byte))
		if err != nil {
			continue
		}
		if _, ok := found[m]; !ok && !skip[m] {
			out = append(out, k)
		}
	}
	return out
}

// prefetchMax bounds the objects a walk looks up in batches before it visits them; past
// it the walk falls back to one lookup per object.
const prefetchMax = 1 << 17

type prefetched struct {
	info  Info
	ok    bool
	codec cid.Codec
	data  []byte // structure block bytes (nil for blobs and missing objects)
}

type prefetchSet struct{ objs map[cid.Multihash]prefetched }

// prefetch looks up root's reachable objects level by level (HasMany, then GetMany for
// held DRISL structure under links), so a depth-first walk can visit them from memory in
// its own order. links nil: only root.
func (s *Store) prefetch(ctx context.Context, root cid.CID, links LinksFunc) (prefetchSet, error) {
	ps := prefetchSet{objs: map[cid.Multihash]prefetched{}}
	frontier := []cid.CID{root}
	for len(frontier) > 0 && len(ps.objs) < prefetchMax {
		if err := ctx.Err(); err != nil {
			return ps, err
		}
		var level []cid.CID
		for _, c := range frontier {
			if _, ok := ps.objs[c.Multihash()]; !ok {
				ps.objs[c.Multihash()] = prefetched{} // dedupes the level; set below
				level = append(level, c)
			}
		}
		has, err := s.HasMany(level)
		if err != nil {
			return ps, err
		}
		var structs []cid.CID
		for _, c := range level {
			info, ok := has[c.Multihash()]
			ps.objs[c.Multihash()] = prefetched{info: info, ok: ok}
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
			b, ok := blocks[c.Multihash()]
			if !ok || b.Codec != c.Codec() {
				continue // the walk reads it itself
			}
			e := ps.objs[c.Multihash()]
			e.codec, e.data = b.Codec, b.Data
			ps.objs[c.Multihash()] = e
			kids, err := links(c, b.Data)
			if err != nil {
				continue // the walk reports it in order
			}
			frontier = append(frontier, kids...)
		}
	}
	return ps, nil
}
