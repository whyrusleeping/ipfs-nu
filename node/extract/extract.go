// Package extract writes a tree walk to a destination directory safely: it refuses path
// traversal, absolute paths and unsafe names, never follows symlinks already present in
// the destination, skips symlink entries (reported, not written), reports names the
// filesystem maps onto another entry (case-insensitive or Unicode-normalizing volumes)
// instead of silently merging them, writes files atomically (temp file + rename) and
// limits modes to 0644/0755. `nu get -o` uses it; the tree walker that feeds it comes
// from the store.
package extract

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"

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
//
// The walk is iterated twice: once to validate every path (X1: a tree with any unsafe
// path is refused before anything is written), then to write. Every write goes through
// an os.Root at dest, so a symlink swapped into the destination while the extraction
// runs still cannot redirect a write outside it (0077).
//
// Paths, directories and name collisions are handled in walk order; file bytes are
// written by fileWorkers goroutines, so Entry.Open must be safe for concurrent use. A name
// is checked for collisions only after every in-flight file it could fold onto has been
// renamed into place.
func Extract(ctx context.Context, dest string, walk Walk) (*Report, error) {
	dest, err := filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	rep := &Report{}
	for e, err := range walk {
		if err != nil {
			return rep, err
		}
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if _, err := safeRelPath(e.Path); err != nil {
			return rep, err
		}
	}
	var root *os.Root
	defer func() {
		if root != nil {
			root.Close()
		}
	}()
	openDest := func() error {
		if root != nil {
			return nil
		}
		if err := ensureDir(nil, dest, true); err != nil {
			return err
		}
		r, err := os.OpenRoot(dest)
		if err != nil {
			return err
		}
		root = r
		return nil
	}
	x := &extraction{written: map[string]bool{}, listed: map[string]map[string]bool{}, checked: map[string]bool{},
		rep: rep, pending: map[string]map[string]bool{}, pendingOther: map[string]bool{}}
	defer x.closeUnsynced()
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan fileJob)
	var workers sync.WaitGroup
	defer func() { close(jobs); workers.Wait() }() // before closeUnsynced
	for range fileWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := range jobs {
				if wctx.Err() == nil {
					x.record(writeFile(wctx, root, j.name, j.existing, j.e, x))
					if x.failed() != nil {
						cancel()
					}
				}
				x.inflight.Done()
			}
		}()
	}
	fail := func(err error) (*Report, error) {
		cancel()
		x.inflight.Wait()
		if werr := x.failed(); werr != nil {
			err = werr // the write's own failure, not the cancellation it caused
		}
		return rep, err
	}
	for e, err := range walk {
		if err != nil {
			return fail(err)
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if err := x.failed(); err != nil {
			return fail(err)
		}
		rel, err := safeRelPath(e.Path)
		if err != nil {
			return fail(err)
		}
		if rel == "" {
			// The root entry.
			switch e.Kind {
			case KindDir:
				if err := openDest(); err != nil {
					return fail(err)
				}
				rep.Dirs++
				continue
			case KindFile:
				parent, err := os.OpenRoot(filepath.Dir(dest))
				if err != nil {
					return fail(err)
				}
				fi, _ := parent.Lstat(filepath.Base(dest))
				n, err := writeFile(ctx, parent, filepath.Base(dest), fi, e, nil)
				parent.Close()
				if err != nil {
					return fail(err)
				}
				x.record(n, nil)
				continue
			default:
				rep.Skipped = append(rep.Skipped, Skipped{Path: e.Path, Reason: "root is a " + string(e.Kind)})
				continue
			}
		}
		if err := openDest(); err != nil {
			return fail(err)
		}
		name := filepath.FromSlash(rel)
		if parent := x.underSkipped(rel); parent != "" {
			rep.Skipped = append(rep.Skipped, Skipped{Path: e.Path, Reason: "inside skipped directory " + parent})
			continue
		}
		if dir := filepath.Dir(name); !x.checked[dir] {
			if err := checkNoSymlinks(root, dir); err != nil {
				return fail(err)
			}
			x.checked[dir] = x.written[dir] // a directory this run made or reused stays checked
		}
		var existing os.FileInfo // name's Lstat, nil if absent
		if e.Kind == KindDir || e.Kind == KindFile {
			if x.mayFoldOntoPending(name) {
				x.drain()
			}
			other, fi, err := x.collides(root, name)
			if err != nil {
				return fail(err)
			}
			existing = fi
			if other != "" {
				rep.Skipped = append(rep.Skipped, Skipped{Path: e.Path, Reason: "name maps onto existing entry " + other + " on this filesystem (case or Unicode normalization)"})
				if e.Kind == KindDir {
					x.skippedDirs = append(x.skippedDirs, rel+"/")
				}
				continue
			}
		}
		switch e.Kind {
		case KindDir:
			if err := ensureDir(root, name, false); err != nil {
				return fail(err)
			}
			x.wrote(name)
			rep.Dirs++
		case KindFile:
			if e.Open == nil {
				return fail(apiv0.Errorf(apiv0.CodeFailed, "file entry %q has no bytes", e.Path))
			}
			x.dispatch(name)
			select {
			case jobs <- fileJob{name, existing, e}:
			case <-wctx.Done():
				x.inflight.Done()
				return fail(ctx.Err())
			}
			x.wrote(name)
		case KindSymlink:
			rep.Skipped = append(rep.Skipped, Skipped{Path: e.Path, Reason: "symlinks are not extracted (target " + e.Target + ")"})
		default:
			rep.Skipped = append(rep.Skipped, Skipped{Path: e.Path, Reason: "unknown entry kind " + string(e.Kind)})
		}
	}
	x.inflight.Wait()
	if err := x.failed(); err != nil {
		return rep, err
	}
	return rep, x.syncUnsynced()
}

// fileWorkers is the number of files written at once. Each file costs a store read and a
// few metadata syscalls (create, rename) that serialize poorly; overlapping them is what
// makes trees of many small files fast to write out.
const fileWorkers = 16

// fileJob is one file for the workers: its name under the root, its Lstat (nil if
// absent) and the entry.
type fileJob struct {
	name     string
	existing os.FileInfo
	e        Entry
}

// record counts a worker's written file, or keeps its error (the first one).
func (x *extraction) record(n int64, err error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if err != nil {
		if x.err == nil {
			x.err = err
		}
		return
	}
	x.rep.Files++
	x.rep.Bytes += n
}

func (x *extraction) failed() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.err
}

// dispatch notes a file handed to the workers: until it is renamed into place, another
// name the filesystem might fold onto it cannot be checked (collides).
func (x *extraction) dispatch(name string) {
	x.inflight.Add(1)
	dir, base := filepath.Dir(name), filepath.Base(name)
	if k, ok := foldKey(base); ok {
		if x.pending[dir] == nil {
			x.pending[dir] = map[string]bool{}
		}
		x.pending[dir][k] = true
	} else {
		x.pendingOther[dir] = true
	}
}

// mayFoldOntoPending reports whether name could be the same file, on a case-folding or
// normalizing volume, as one still being written. ASCII names fold by case alone; any
// other name (Unicode folding and normalization, e.g. U+212A KELVIN SIGN folds to "k")
// may match anything.
func (x *extraction) mayFoldOntoPending(name string) bool {
	dir, base := filepath.Dir(name), filepath.Base(name)
	k, ok := foldKey(base)
	return x.pendingOther[dir] || (!ok && len(x.pending[dir]) > 0) || x.pending[dir][k]
}

// drain waits for every dispatched file to be written.
func (x *extraction) drain() {
	x.inflight.Wait()
	clear(x.pending)
	clear(x.pendingOther)
}

// foldKey is an ASCII name as case-insensitive filesystems compare it: lower case,
// without the trailing dots and spaces Windows filesystems drop. ok is false for names
// with other bytes.
func foldKey(name string) (string, bool) {
	for i := 0; i < len(name); i++ {
		if name[i] >= 0x80 {
			return "", false
		}
	}
	return strings.TrimRight(strings.ToLower(name), ". "), true
}

// extraction tracks what this run wrote so that filesystem name transformations are
// detected: a path that exists although this run never wrote it, and whose exact name is
// absent from its directory's listing, has been mapped by the filesystem onto another
// entry (APFS/NTFS case folding, HFS+ NFD normalization). Writing it would silently
// replace or merge content, so it is reported instead. Names are relative to the root.
type extraction struct {
	written     map[string]bool            // names written by this run
	listed      map[string]map[string]bool // directory → names present (lazily read)
	skippedDirs []string                   // tree-relative prefixes ("a/b/") whose contents are skipped
	checked     map[string]bool            // directories already checked for symlinks on the way

	// Files are written by workers (fileWorkers): inflight counts the dispatched ones, and
	// pending/pendingOther note, per directory, the names dispatched since the last drain.
	inflight     sync.WaitGroup
	pending      map[string]map[string]bool // directory → foldKey of pending names
	pendingOther map[string]bool            // directory → a pending name without a foldKey

	mu       sync.Mutex // guards the fields below, shared with the workers
	rep      *Report
	err      error
	unsynced []*os.File // new files renamed into place, still open, not yet synced
}

// maxUnsynced bounds the new files kept open for one parallel sync (0084).
const maxUnsynced = 256

// deferSync takes a new file that is already renamed into place; files are synced
// together, in parallel, because one fsync at a time costs milliseconds per file.
func (x *extraction) deferSync(f *os.File) error {
	x.mu.Lock()
	x.unsynced = append(x.unsynced, f)
	if len(x.unsynced) < maxUnsynced {
		x.mu.Unlock()
		return nil
	}
	batch := x.unsynced
	x.unsynced = nil
	x.mu.Unlock()
	return syncFiles(batch)
}

// syncUnsynced fsyncs the pending new files concurrently and closes them.
func (x *extraction) syncUnsynced() error {
	x.mu.Lock()
	batch := x.unsynced
	x.unsynced = nil
	x.mu.Unlock()
	return syncFiles(batch)
}

func syncFiles(files []*os.File) error {
	errs := make([]error, len(files))
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = errors.Join(f.Sync(), f.Close())
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// closeUnsynced closes pending files after a failed extraction.
func (x *extraction) closeUnsynced() {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, f := range x.unsynced {
		f.Close()
	}
	x.unsynced = nil
}

func (x *extraction) wrote(name string) {
	x.written[name] = true
	if names := x.listed[filepath.Dir(name)]; names != nil {
		names[filepath.Base(name)] = true
	}
}

// collides returns the existing entry name that name would map onto, or "", and name's
// Lstat when it exists (nil when absent or unreadable: the write reports that).
func (x *extraction) collides(root *os.Root, name string) (string, os.FileInfo, error) {
	fi, err := root.Lstat(name)
	if err != nil {
		return "", nil, nil
	}
	if x.written[name] {
		return "", fi, nil
	}
	dir, base := filepath.Dir(name), filepath.Base(name)
	names := x.listed[dir]
	if names == nil {
		d, err := root.Open(dir)
		if err != nil {
			return "", nil, err
		}
		entries, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			return "", nil, err
		}
		names = make(map[string]bool, len(entries))
		for _, de := range entries {
			names[de.Name()] = true
		}
		x.listed[dir] = names
	}
	if names[base] {
		return "", fi, nil // a pre-existing file of exactly this name: replaced, as documented
	}
	// Name the entry the filesystem resolved it to: the listed name that is the same
	// file (case folding, Unicode normalization or any other mapping), else a case-only
	// match.
	for n := range names {
		if other, err := root.Lstat(filepath.Join(dir, n)); err == nil && os.SameFile(fi, other) {
			return n, fi, nil
		}
	}
	for n := range names {
		if strings.EqualFold(n, base) {
			return n, fi, nil
		}
	}
	return "(a differently spelled name)", fi, nil
}

// underSkipped returns the skipped directory prefix containing rel, or "".
func (x *extraction) underSkipped(rel string) string {
	for _, d := range x.skippedDirs {
		if strings.HasPrefix(rel, d) {
			return strings.TrimSuffix(d, "/")
		}
	}
	return ""
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

// checkNoSymlinks walks from the root down to dir (relative) and fails on any symlink
// component. The os.Root already refuses to leave dest; this keeps X2's stricter rule
// (no symlink at all on the way) with a clear error.
func checkNoSymlinks(root *os.Root, dir string) error {
	cur := ""
	for _, seg := range strings.Split(filepath.ToSlash(dir), "/") {
		if seg == "." || seg == "" {
			continue
		}
		cur = filepath.Join(cur, seg)
		fi, err := root.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil // nothing below exists yet; it will be created by us
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return symlinkErr(filepath.Join(root.Name(), cur))
		}
		if !fi.IsDir() {
			return apiv0.Errorf(apiv0.CodeConflict, "%s exists and is not a directory", filepath.Join(root.Name(), cur))
		}
	}
	return nil
}

func symlinkErr(p string) error {
	return apiv0.Errorf(apiv0.CodeInvalidArgument, "unsafe path: %s is a symlink", p).
		WithAction("remove the symlink from the destination or extract somewhere else")
}

// ensureDir creates dir (not its parents unless parentsOK) and refuses symlinks. With a
// root, dir is relative to it; without (the destination itself), it is absolute.
func ensureDir(root *os.Root, dir string, parentsOK bool) error {
	lstat, mkdir, mkdirAll, show := os.Lstat, os.Mkdir, os.MkdirAll, dir
	if root != nil {
		lstat, mkdir, mkdirAll, show = root.Lstat, root.Mkdir, root.MkdirAll, filepath.Join(root.Name(), dir)
	}
	fi, err := lstat(dir)
	switch {
	case err == nil:
		if fi.Mode()&os.ModeSymlink != 0 {
			return symlinkErr(show)
		}
		if !fi.IsDir() {
			return apiv0.Errorf(apiv0.CodeConflict, "%s exists and is not a directory", show)
		}
		return nil
	case errors.Is(err, os.ErrNotExist):
		if parentsOK {
			return mkdirAll(dir, DirMode)
		}
		return mkdir(dir, DirMode)
	}
	return err
}

// writeFile writes e's bytes to name under root atomically (temp file + rename, both
// inside the root). A file that replaces an existing one is synced before the rename, so
// the replacement is atomic across a crash. A new file is renamed at once and handed to x
// to be synced with others before the extraction succeeds; with a nil x it is synced
// before the rename too. existing is name's Lstat (nil when absent). It returns the bytes
// written and is safe to run concurrently for different names.
func writeFile(ctx context.Context, root *os.Root, name string, existing os.FileInfo, e Entry, x *extraction) (int64, error) {
	if e.Open == nil {
		return 0, apiv0.Errorf(apiv0.CodeFailed, "file entry %q has no bytes", e.Path)
	}
	show := filepath.Join(root.Name(), name)
	replacing := false
	if fi := existing; fi != nil {
		replacing = true
		if fi.Mode()&os.ModeSymlink != 0 {
			return 0, symlinkErr(show)
		}
		if fi.IsDir() {
			return 0, apiv0.Errorf(apiv0.CodeConflict, "%s exists and is a directory", show)
		}
	}
	src, err := e.Open(ctx)
	if err != nil {
		return 0, err
	}
	defer src.Close()
	mode := FileMode
	if e.Mode&0o111 != 0 {
		mode = ExecMode
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmpName := filepath.Join(filepath.Dir(name), ".nu-extract-"+hex.EncodeToString(rnd[:]))
	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	renamed := false
	defer func() {
		if !renamed {
			root.Remove(tmpName)
		}
	}()
	later := x != nil && !replacing
	n, err := io.Copy(tmp, src)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil && !later {
		err = tmp.Sync()
	}
	if !later {
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		err = root.Rename(tmpName, name)
		renamed = err == nil
	}
	if err != nil {
		if later {
			tmp.Close()
		}
		return 0, err
	}
	if later {
		return n, x.deferSync(tmp)
	}
	return n, nil
}
