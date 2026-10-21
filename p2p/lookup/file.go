package lookup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/whyrusleeping/nu/p2p/wire"
)

const maxCacheFile = 1 << 20

type FileStore struct{ Path string }
type cacheFile struct {
	Version int     `json:"v"`
	Entries []Entry `json:"entries"`
}

func (f FileStore) Load(ctx context.Context) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stat, err := os.Lstat(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm() != 0600 {
		return nil, errors.New("lookup: cache must be a regular 0600 file")
	}
	file, err := os.Open(f.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, maxCacheFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxCacheFile {
		return nil, wire.ErrLimit
	}
	return DecodeCache(b, maxCacheFile)
}

// DecodeCache bounds the file and validates its version. NewCache validates
// entry semantics before installing the snapshot.
func DecodeCache(b []byte, maxBytes int) ([]Entry, error) {
	if maxBytes < 0 || len(b) > maxBytes {
		return nil, wire.ErrLimit
	}
	var data cacheFile
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	if data.Version != 1 {
		return nil, wire.ErrFormat
	}
	if len(data.Entries) > 8192 {
		return nil, wire.ErrLimit
	}
	return data.Entries, nil
}
func (f FileStore) Save(ctx context.Context, entries []Entry) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := json.Marshal(cacheFile{Version: 1, Entries: entries})
	if err != nil {
		return err
	}
	if len(b) > maxCacheFile {
		return wire.ErrLimit
	}
	dir := filepath.Dir(f.Path)
	file, err := os.CreateTemp(dir, ".peer-cache-*")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if err = file.Chmod(0600); err != nil {
		return err
	}
	if _, err = file.Write(b); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), f.Path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
