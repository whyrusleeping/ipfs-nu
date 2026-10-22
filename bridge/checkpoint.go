package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// CheckpointSource durably retains whole source blocks only after their original CID
// has been verified. The cache is scoped to one import operation and can be reopened
// after a process restart; corrupt cache entries are discarded and fetched again.
type CheckpointSource struct {
	Dir      string
	Upstream BlockSource
}

func (s CheckpointSource) GetBlock(ctx context.Context, id IPFSCID) ([]byte, error) {
	if s.Dir == "" {
		return nil, fmt.Errorf("bridge: checkpoint directory required")
	}
	key := sha256.Sum256(id.Bytes())
	name := hex.EncodeToString(key[:])
	path := filepath.Join(s.Dir, name+".block")
	if b, err := os.ReadFile(path); err == nil {
		if id.Verify(b) == nil {
			return b, nil
		}
		_ = os.Remove(path)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if s.Upstream == nil {
		return nil, fmt.Errorf("bridge: verified checkpoint missing and no source available for %s", id.String())
	}
	b, err := s.Upstream.GetBlock(ctx, id)
	if err != nil {
		return nil, err
	}
	if err = id.Verify(b); err != nil {
		return nil, fmt.Errorf("bridge: verify %s: %w", id.String(), err)
	}
	if err = os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(s.Dir, ".checkpoint-*.tmp")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return nil, err
	}
	if err = os.Rename(tmp, path); err != nil {
		return nil, err
	}
	d, err := os.Open(s.Dir)
	if err == nil {
		err = d.Sync()
		_ = d.Close()
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}
