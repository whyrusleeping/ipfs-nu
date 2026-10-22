package atproto

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
)

// Sessions stores OAuth sessions as 0600 JSON files in one 0700 directory
// ($NU_PATH/auth), one per DID. Tokens never leave the machine; the directory is never
// part of bundles or exports.
type Sessions struct {
	dir string
	mu  sync.Mutex
}

// OpenSessions creates dir if needed.
func OpenSessions(dir string) (*Sessions, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Sessions{dir: dir}, nil
}

func (s *Sessions) path(did string) string {
	return filepath.Join(s.dir, strings.NewReplacer(":", "_", "/", "_", "%", "_").Replace(did)+".json")
}

// Load returns the session of did, or a not_found error naming `nu login`.
func (s *Sessions) Load(did string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path(did))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil || sess.DID != did {
		return nil, fmt.Errorf("session file %s is unreadable", s.path(did))
	}
	return &sess, nil
}

// ErrNoSession reports no stored session.
var ErrNoSession = apiv0.Errorf(apiv0.CodeForbidden, "not logged in").WithAction("run `nu login <handle>` first")

// Save writes sess (0600, atomically).
func (s *Sessions) Save(sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.Updated.IsZero() {
		sess.Updated = time.Now()
	}
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	p := s.path(sess.DID)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Delete removes the session of did (absent is fine).
func (s *Sessions) Delete(did string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(did))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// List returns every stored session, sorted by DID.
func (s *Sessions) List() ([]*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var sess Session
		if json.Unmarshal(data, &sess) == nil && sess.DID != "" {
			out = append(out, &sess)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DID < out[j].DID })
	return out, nil
}

// Default returns the only session, or an error naming the choice when there are
// several (or none).
func (s *Sessions) Default() (*Session, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	switch len(all) {
	case 0:
		return nil, ErrNoSession
	case 1:
		return all[0], nil
	}
	var dids []string
	for _, x := range all {
		dids = append(dids, x.DID)
	}
	return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%d accounts are logged in (%s)", len(all), strings.Join(dids, ", ")).
		WithAction("pass --as <handle-or-did>")
}
