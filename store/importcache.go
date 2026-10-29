package store

import (
	"database/sql"
	"errors"

	"github.com/whyrusleeping/nu/core/cid"
)

// Source identifies a local file as seen when it was added: its absolute path and the
// stat fields that change when its contents might have (size, mtime, inode).
type Source struct {
	Path  string
	Size  int64
	Mtime int64 // UnixNano
	Inode uint64
}

// ImportCacheGet returns the blob CID recorded for src under hash h when the recorded
// stat fields still match and the blob is still held (GC may have removed it). A --nocopy
// source that changed is reported absent by Has, so a stale reference is a miss.
func (s *Store) ImportCacheGet(src Source, h cid.Hash) (cid.CID, Info, bool, error) {
	var b []byte
	var size, mtime int64
	var inode uint64
	err := s.queryRow(`SELECT cid, size, mtime, inode FROM import_cache WHERE path = ? AND hash = ?`,
		src.Path, int(h)).Scan(&b, &size, &mtime, &inode)
	if errors.Is(err, sql.ErrNoRows) {
		return cid.CID{}, Info{}, false, nil
	}
	if err != nil {
		return cid.CID{}, Info{}, false, err
	}
	if size != src.Size || mtime != src.Mtime || inode != src.Inode {
		return cid.CID{}, Info{}, false, nil
	}
	c, err := cid.Decode(b)
	if err != nil {
		return cid.CID{}, Info{}, false, nil
	}
	info, ok, err := s.Has(c)
	if err != nil || !ok || !info.Complete {
		return cid.CID{}, Info{}, false, err
	}
	return c, info, true, nil
}

// ImportCachePut records that src hashed to c under h.
func (s *Store) ImportCachePut(src Source, h cid.Hash, c cid.CID) error {
	_, err := s.exec(importCachePut, src.Path, int(h), src.Size, src.Mtime, src.Inode, c.Bytes(), s.nowUnix())
	return err
}

const importCachePut = `INSERT OR REPLACE INTO import_cache (path, hash, size, mtime, inode, cid, used)
	VALUES (?, ?, ?, ?, ?, ?, ?)`
