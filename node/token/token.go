// Package token manages the API credentials of producers on other machines (rule A8 of
// docs/operations.md): bearer tokens scoped to item pushes into named collections, one
// 0600 JSON file per token under $NU_PATH/tokens. Rotating rewrites the secret; deleting
// the file (or `nu api token rm`) revokes it, effective at the next request.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Dir is the token directory under $NU_PATH.
const Dir = "tokens"

// Prefix starts every token, so leaked tokens are recognisable.
const Prefix = "nu_pt_"

// ScopePush is the only scope so far: item pushes, uploads, removals and push status
// of the listed collections.
const ScopePush = "collection-push"

// ErrNotFound and ErrExists report a missing or duplicate token name.
var (
	ErrNotFound = errors.New("token: no such token")
	ErrExists   = errors.New("token: a token with this name exists")
)

// Token is one credential. Secret is the bearer token itself.
type Token struct {
	Name        string    `json:"name"`
	Secret      string    `json:"token"`
	Scope       string    `json:"scope"`
	Collections []string  `json:"collections"`
	Created     time.Time `json:"created"`
}

// Allows reports whether the token may push into collection name.
func (t *Token) Allows(name string) bool {
	return t.Scope == ScopePush && slices.Contains(t.Collections, name)
}

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func file(dir, name string) (string, error) {
	if !validName.MatchString(name) {
		return "", fmt.Errorf("token name %q must match [a-z0-9][a-z0-9-]{0,63}", name)
	}
	return filepath.Join(dir, name+".json"), nil
}

func secret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return Prefix + base64.RawURLEncoding.EncodeToString(b)
}

func write(path string, t *Token, excl bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if excl {
		flags |= os.O_EXCL
	}
	tmp := path
	if !excl {
		tmp = path + ".tmp"
	}
	f, err := os.OpenFile(tmp, flags, 0o600)
	if errors.Is(err, os.ErrExist) {
		return ErrExists
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if tmp != path {
		return os.Rename(tmp, path)
	}
	return nil
}

// Create makes a push token for collections.
func Create(dir, name string, collections []string) (*Token, error) {
	path, err := file(dir, name)
	if err != nil {
		return nil, err
	}
	if len(collections) == 0 {
		return nil, errors.New("a push token needs at least one collection")
	}
	t := &Token{Name: name, Secret: secret(), Scope: ScopePush, Collections: slices.Sorted(slices.Values(collections)), Created: time.Now().UTC()}
	return t, write(path, t, true)
}

// Get reads one token.
func Get(dir, name string) (*Token, error) {
	path, err := file(dir, name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	return &t, nil
}

// Rotate replaces a token's secret, keeping its name and scope.
func Rotate(dir, name string) (*Token, error) {
	t, err := Get(dir, name)
	if err != nil {
		return nil, err
	}
	t.Secret, t.Created = secret(), time.Now().UTC()
	path, _ := file(dir, name)
	return t, write(path, t, false)
}

// Remove revokes a token.
func Remove(dir, name string) error {
	path, err := file(dir, name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// List returns every token, by name.
func List(dir string) ([]Token, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Token
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		t, err := Get(dir, name)
		if err != nil {
			continue
		}
		out = append(out, *t)
	}
	return out, nil
}

// Lookup finds the token whose secret is presented, comparing digests in constant time
// against every stored token. Tokens are read on every call, so a removal or rotation
// applies to the next request without a restart.
func Lookup(dir, presented string) (*Token, bool) {
	if !strings.HasPrefix(presented, Prefix) {
		return nil, false
	}
	ts, err := List(dir)
	if err != nil {
		return nil, false
	}
	want := sha256.Sum256([]byte(presented))
	var found *Token
	for i := range ts {
		got := sha256.Sum256([]byte(ts[i].Secret))
		if subtle.ConstantTimeCompare(want[:], got[:]) == 1 {
			found = &ts[i]
		}
	}
	return found, found != nil
}
