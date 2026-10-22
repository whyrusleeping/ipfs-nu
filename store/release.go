package store

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"

	"github.com/whyrusleeping/nu/core/cid"
)

// Scrub and release support for cluster members (docs/cluster.md "Scrub and audit",
// docs/operations.md S1–S3).

// Verify re-reads a held object and checks it against its CID: a structure block or
// inline blob by its bytes, a stored blob file by re-hashing it. ok is false for
// corrupt bytes (bit rot); objects not held, partial blobs and --nocopy references
// (re-verified by Has when their source changes) report ok.
func (s *Store) Verify(c cid.CID) (ok bool, err error) {
	m := c.Multihash()
	var data []byte
	err = s.db.QueryRow(`SELECT data FROM blocks WHERE mh = ?`, mhKey(m)).Scan(&data)
	if err == nil {
		return cid.Sum(m.Hash, data) == m, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	row, err := s.blobRow(m)
	if errors.Is(err, ErrNotFound) || (err == nil && row.srcPath != "") {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	f, err := os.Open(s.blobPath(m))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // the row says held, the file is gone
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := m.Hash.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return false, err
	}
	return uint64(n) == row.size && cid.FromHasher(m.Hash, h) == m, nil
}

// Drop removes held objects (complete blobs and blocks) unless a pin that keep accepts
// reaches them; it reports the objects removed. Pins keep rejects (a cluster member's
// dataset pins, whose surplus blobs it releases per blob) do not protect. It runs under
// the GC lock, so no concurrent pin or GC interleaves. A --nocopy source file is never
// deleted, only its reference.
func (s *Store) Drop(ctx context.Context, mhs []cid.Multihash, keep func(Pin) bool) ([]cid.Multihash, error) {
	if len(mhs) == 0 {
		return nil, nil
	}
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	pins, err := s.Pins()
	if err != nil {
		return nil, err
	}
	want := map[cid.Multihash]bool{}
	for _, m := range mhs {
		want[m] = true
	}
	for _, p := range pins {
		if keep != nil && !keep(p) {
			continue
		}
		if _, err := s.Walk(ctx, p, func(c cid.CID, _ bool) error {
			delete(want, c.Multihash())
			return nil
		}); err != nil {
			return nil, err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var out []cid.Multihash
	var files []string
	for _, m := range mhs {
		if !want[m] {
			continue
		}
		delete(want, m) // once
		n := int64(0)
		for _, table := range []string{"blocks", "blobs"} {
			res, err := tx.Exec(`DELETE FROM `+table+` WHERE mh = ?`, mhKey(m))
			if err != nil {
				return nil, err
			}
			k, _ := res.RowsAffected()
			n += k
			if table == "blobs" && k > 0 {
				files = append(files, s.blobPath(m), s.blobPath(m)+".obao")
			}
		}
		if n > 0 {
			out = append(out, m)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, f := range files {
		_ = os.Remove(f)
	}
	return out, nil
}
