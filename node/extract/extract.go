// Package extract writes a tree walk to a destination directory safely: it refuses path
// traversal, absolute paths and unsafe names, never follows symlinks already present in
// the destination, skips symlink entries (reported, not written), writes files atomically
// (temp file + rename) and limits modes to 0644/0755. `nu get -o` uses it; the tree walker
// that feeds it comes from the store.
package extract

import (
	"context"
	"errors"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"

	"github.com/whyrusleeping/nu/api/apiv0"
)

// Kind of a walked entry.
type Kind string

const (
	KindFile    Kind = "file"
	KindDir     Kind = "dir"
	KindSymlink Kind = "symlink"
)

// Entry is one node of a tree walk, in pre-order (a directory before its contents).
type Entry struct {
	// Path is slash-separated and relative to the tree root; "" is the root itself.
	Path string
	Kind Kind
	// Mode is the tree's mode bits, if any; only the executable bit is honoured.
	Mode uint32
	Size int64
	// Open returns the file's bytes (KindFile only).
	Open func(ctx context.Context) (io.ReadCloser, error)
	// Target is a symlink's target (KindSymlink only); recorded in the report, never created.
	Target string
}

// Walk yields entries; a non-nil error ends the extraction.
type Walk = iter.Seq2[Entry, error]

// Report summarizes an extraction.
type Report struct {
	Files   int       `json:"files"`
	Dirs    int       `json:"dirs"`
	Bytes   int64     `json:"bytes"`
	Skipped []Skipped `json:"skipped,omitempty"`
}

// Skipped is an entry that was not written and why.
type Skipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Modes written, regardless of what the tree says beyond the executable bit.
const (
	FileMode os.FileMode = 0o644
	ExecMode os.FileMode = 0o755
	DirMode  os.FileMode = 0o755
)

// Extract writes walk under dest. dest is created if absent (its parents must exist) and
// may already contain files, which are replaced atomically; existing directories are
// reused. Any symlink met while descending into dest is an error, so a hostile tree or a
// previously extracted tree cannot redirect writes outside dest. A walk whose root
// entry (Path "") is a file is written to dest itself, which must not be a directory.
func Extract(ctx context.Context, dest string, walk Walk) (*Report, error) {
	dest, err := filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	rep := &Report{}
	rootPrepared := false
	for e, err := range walk {
		if err != nil {
			return rep, err
		}
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rel, err := safeRelPath(e.Path)
		if err != nil {
			return rep, err
		}
		if rel == "" {
			// The root entry.
			switch e.Kind {
			case KindDir:
				if err := ensureDir(dest, true); err != nil {
					return rep, err
				}
				rootPrepared = true
				rep.Dirs++
				continue
			case KindFile:
				if err := writeFile(ctx, dest, e, rep); err != nil {
					return rep, err
				}
				continue
			default:
				rep.Skipped = append(rep.Skipped, Skipped{Path: e.Path, Reason: "root is a " + string(e.Kind)})
				continue
			}
		}
		if !rootPrepared {
			if err := ensureDir(dest, true); err != nil {
				return rep, err
			}
			rootPrepared = true
		}
		full := filepath.Join(dest, filepath.FromSlash(rel))
		if err := checkNoSymlinks(dest, filepath.Dir(full)); err != nil {
			return rep, err
		}
		switch e.Kind {
		case KindDir:
			if err := ensureDir(full, false); err != nil {
				return rep, err
			}
			rep.Dirs++
		case KindFile:
			if err := writeFile(ctx, full, e, rep); err != nil {
				return rep, err
			}
		case KindSymlink:
			rep.Skipped = append(rep.Skipped, Skipped{Path: e.Path, Reason: "symlinks are not extracted (target " + e.Target + ")"})
		default:
			rep.Skipped = append(rep.Skipped, Skipped{Path: e.Path, Reason: "unknown entry kind " + string(e.Kind)})
		}
	}
	return rep, nil
}

// safeRelPath validates a tree path and returns it slash-separated without surrounding
// slashes. It rejects absolute paths, ".", "..", empty segments, NUL and backslashes.
func safeRelPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "/") {
		return "", unsafe(p, "absolute path")
	}
	for _, seg := range strings.Split(p, "/") {
		switch {
		case seg == "" || seg == ".":
			return "", unsafe(p, "empty or '.' segment")
		case seg == "..":
			return "", unsafe(p, "'..' segment")
		case strings.ContainsAny(seg, "\x00\\"):
			return "", unsafe(p, "NUL or backslash in name")
		case len(seg) > 255:
			return "", unsafe(p, "name longer than 255 bytes")
		}
	}
	return p, nil
}

func unsafe(p, why string) error {
	return apiv0.Errorf(apiv0.CodeInvalidArgument, "unsafe path %q: %s", p, why).
		WithAction("the tree contains an entry that cannot be written safely; extract it with a tool that handles it, or report the tree")
}

// checkNoSymlinks walks from dest down to dir and fails on any symlink component.
func checkNoSymlinks(dest, dir string) error {
	rel, err := filepath.Rel(dest, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "unsafe path: %s is outside %s", dir, dest)
	}
	cur := dest
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == "." || seg == "" {
			continue
		}
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil // nothing below exists yet; it will be created by us
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "unsafe path: %s is a symlink", cur).
				WithAction("remove the symlink from the destination or extract somewhere else")
		}
		if !fi.IsDir() {
			return apiv0.Errorf(apiv0.CodeConflict, "%s exists and is not a directory", cur)
		}
	}
	return nil
}

// ensureDir creates dir (not its parents unless parentsOK) and refuses symlinks.
func ensureDir(dir string, parentsOK bool) error {
	fi, err := os.Lstat(dir)
	switch {
	case err == nil:
		if fi.Mode()&os.ModeSymlink != 0 {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "unsafe path: %s is a symlink", dir).
				WithAction("remove the symlink from the destination or extract somewhere else")
		}
		if !fi.IsDir() {
			return apiv0.Errorf(apiv0.CodeConflict, "%s exists and is not a directory", dir)
		}
		return nil
	case errors.Is(err, os.ErrNotExist):
		if parentsOK {
			return os.MkdirAll(dir, DirMode)
		}
		return os.Mkdir(dir, DirMode)
	}
	return err
}

// writeFile writes e's bytes to full atomically.
func writeFile(ctx context.Context, full string, e Entry, rep *Report) error {
	if e.Open == nil {
		return apiv0.Errorf(apiv0.CodeFailed, "file entry %q has no bytes", e.Path)
	}
	if fi, err := os.Lstat(full); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "unsafe path: %s is a symlink", full).
				WithAction("remove the symlink from the destination or extract somewhere else")
		}
		if fi.IsDir() {
			return apiv0.Errorf(apiv0.CodeConflict, "%s exists and is a directory", full)
		}
	}
	src, err := e.Open(ctx)
	if err != nil {
		return err
	}
	defer src.Close()
	mode := FileMode
	if e.Mode&0o111 != 0 {
		mode = ExecMode
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".nu-extract-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, src)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), full); err != nil {
		return err
	}
	rep.Files++
	rep.Bytes += n
	return nil
}
