// Package repo owns the `$NU_PATH` directory: its layout, config.toml, the daemon lock and
// the address files that let clients find a running daemon. Any command opens the repo and
// initializes it on first use; `nu init` only does it explicitly.
package repo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/whyrusleeping/nu"
)

// File and directory names inside `$NU_PATH`.
const (
	// ConfigFile is the TOML configuration.
	ConfigFile = "config.toml"
	// KeyFile is the node's libp2p identity key. Reserved: the networking track
	// (p2p/wire) generates and loads it; nothing here reads or writes it.
	KeyFile = "node.key"
	// DBFile is the SQLite database (metadata, DRISL blocks, pins, indexes). Reserved for
	// the store.
	DBFile = "nu.db"
	// BlobDir holds blob and outboard files. Reserved for the store.
	BlobDir = "blobs"
	// APIFile holds the URL of the running daemon's API, e.g. "http://127.0.0.1:5171".
	// Written at daemon start, removed at clean shutdown.
	APIFile = "api"
	// GatewayFile holds the URL of the running daemon's gateway.
	GatewayFile = "gateway"
	// LockFile is held (flock) by the running daemon and contains its PID.
	LockFile = "daemon.lock"
	// VersionFile records the layout version for future migrations.
	VersionFile = "version"
)

// LayoutVersion is written to VersionFile at init.
const LayoutVersion = 1

// EnvPath is the environment variable overriding the default directory.
const EnvPath = nu.EnvPrefix + "PATH"

// Permissions: files 0600, directories 0700.
const (
	FileMode os.FileMode = 0o600
	DirMode  os.FileMode = 0o700
)

// Repo is an opened `$NU_PATH`.
type Repo struct {
	root string
}

// DefaultPath returns $NU_PATH, or ~/.nu when unset.
func DefaultPath() (string, error) {
	if p := os.Getenv(EnvPath); p != "" {
		return filepath.Abs(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w (set %s)", err, EnvPath)
	}
	return filepath.Join(home, nu.DefaultDir), nil
}

// Exists reports whether path holds an initialized repo.
func Exists(path string) bool {
	_, err := os.Stat(filepath.Join(path, ConfigFile))
	return err == nil
}

// Open opens the repo at path, initializing it if absent. It returns whether it was created.
func Open(path string) (*Repo, bool, error) {
	if path == "" {
		var err error
		if path, err = DefaultPath(); err != nil {
			return nil, false, err
		}
	}
	r := &Repo{root: path}
	if Exists(path) {
		return r, false, nil
	}
	if err := r.init(); err != nil {
		return nil, false, err
	}
	return r, true, nil
}

func (r *Repo) init() error {
	if err := os.MkdirAll(r.root, DirMode); err != nil {
		return fmt.Errorf("create %s: %w", r.root, err)
	}
	if err := os.Chmod(r.root, DirMode); err != nil {
		return err
	}
	if err := os.MkdirAll(r.Path(BlobDir), DirMode); err != nil {
		return err
	}
	if err := writeFileAtomic(r.Path(VersionFile), []byte(strconv.Itoa(LayoutVersion)+"\n")); err != nil {
		return err
	}
	cfg := DefaultConfig()
	return r.writeConfigStruct(cfg)
}

// Root is the repo directory.
func (r *Repo) Root() string { return r.root }

// Path returns the absolute path of a file or directory inside the repo.
func (r *Repo) Path(name string) string { return filepath.Join(r.root, name) }

// WriteAddrFile writes the URL of a listener (APIFile or GatewayFile).
func (r *Repo) WriteAddrFile(name, url string) error {
	return writeFileAtomic(r.Path(name), []byte(url+"\n"))
}

// ReadAddrFile returns the URL stored in an address file, or "" when absent.
func (r *Repo) ReadAddrFile(name string) (string, error) {
	b, err := os.ReadFile(r.Path(name))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// RemoveAddrFile removes an address file; a missing file is not an error.
func (r *Repo) RemoveAddrFile(name string) error {
	err := os.Remove(r.Path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// writeFileAtomic writes a 0600 file via rename so readers never see a partial file.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(FileMode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
