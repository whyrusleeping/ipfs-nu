package repo

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ErrLocked is returned (wrapped, with the holder's PID when known) when another daemon
// holds the repo lock.
var ErrLocked = errors.New("repo is locked by a running daemon")

// Lock is the held daemon lock.
type Lock struct {
	f *os.File
}

// Lock takes the daemon lock (an flock on LockFile) and records the caller's PID in it.
// It fails with ErrLocked when another process holds it.
func (r *Repo) Lock() (*Lock, error) {
	f, err := os.OpenFile(r.Path(LockFile), os.O_RDWR|os.O_CREATE, FileMode)
	if err != nil {
		return nil, err
	}
	if err := flock(f); err != nil {
		pid, _ := r.LockHolder()
		f.Close()
		if pid > 0 {
			return nil, fmt.Errorf("%w (pid %d)", ErrLocked, pid)
		}
		return nil, ErrLocked
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// LockHolder returns the PID recorded in the lock file, or 0 when absent. It does not
// check whether the lock is actually held; use Lock for that.
func (r *Repo) LockHolder() (int, error) {
	b, err := os.ReadFile(r.Path(LockFile))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, nil
	}
	return pid, nil
}

// Release drops the lock and clears the recorded PID.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = l.f.Truncate(0)
	err := funlock(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
