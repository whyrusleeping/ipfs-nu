package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/node/op"
	"github.com/whyrusleeping/nu/store"
)

// Add validates the request and starts the add operation. A file becomes a MASL file
// node over one blob (or the bare blob with Raw); a directory (Recursive) becomes a tree
// with its io.ipfs.nu.tree header. The result is pinned (explicit, recursive) when Pin is
// set. Unchanged files (same path, size, mtime and inode) are not re-hashed on re-add.
func (n *Local) Add(ctx context.Context, req apiv0.AddRequest) (*apiv0.Operation, error) {
	if req.Path == "" {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "add needs a path").WithAction("give a file or directory path")
	}
	if !filepath.IsAbs(req.Path) {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "add needs an absolute path, not %q", req.Path).
			WithAction("the daemon reads the path itself, so it must not depend on the caller's working directory")
	}
	h := cid.BLAKE3
	switch req.Hash {
	case "", "blake3":
	case "sha256":
		h = cid.SHA256
	default:
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "unknown hash %q", req.Hash).WithAction("use blake3 (default) or sha256")
	}
	if req.NoCopy && h != cid.BLAKE3 {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "--nocopy references are BLAKE3 only").WithAction("drop --hash sha256 or --nocopy")
	}
	fi, err := os.Stat(req.Path)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "cannot read %s: %v", req.Path, err).WithAction("check the path")
	}
	switch {
	case fi.IsDir() && !req.Recursive:
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is a directory", req.Path).WithAction("add it with -r")
	case !fi.IsDir() && !fi.Mode().IsRegular():
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is not a regular file or directory", req.Path)
	case !fi.IsDir() && h == cid.SHA256 && fi.Size() > cid.MaxWholeObject:
		return nil, errTooLargeSHA256(req.Path, fi.Size())
	}
	return n.start("add", req.Path, func(o *op.Operation) (any, error) {
		a := &adder{n: n, o: o, req: req, hash: h}
		if fi.IsDir() {
			return a.dir()
		}
		return a.file(fi)
	}), nil
}

func errTooLargeSHA256(path string, size int64) error {
	return apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is %d bytes; --hash sha256 blobs are limited to %d bytes (8 MiB)", path, size, cid.MaxWholeObject).
		WithAction("add it with the default BLAKE3 hash, which has no size limit")
}

// adder runs one add operation.
type adder struct {
	n    *Local
	o    *op.Operation
	req  apiv0.AddRequest
	hash cid.Hash

	done, total int64
	last        time.Time
}

// progress adds delta bytes, emitting a rate-limited progress event.
func (a *adder) progress(delta int64, force bool) {
	a.done += delta
	if force || time.Since(a.last) > 100*time.Millisecond {
		a.o.Progress(a.done, a.total, "bytes")
		a.last = time.Now()
	}
}

// reader counts bytes through progress and stops on cancellation.
type reader struct {
	a *adder
	r io.Reader
}

func (r reader) Read(p []byte) (int, error) {
	if err := r.a.o.Context().Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	r.a.progress(int64(n), false)
	return n, err
}

// AddBlob stores a blob the tree builder gives it (symlink targets). Regular files go
// through addFile.
func (a *adder) AddBlob(r io.Reader, size int64) (cid.CID, error) {
	return a.n.Store.AddBlobHash(reader{a, r}, size, a.hash)
}

// addFile stores one regular file: from the import cache when unchanged, as a --nocopy
// reference, or by streaming it into the store.
func (a *adder) addFile(path string, f fs.File, fi fs.FileInfo) (cid.CID, error) {
	src := store.Source{Path: path, Size: fi.Size(), Mtime: fi.ModTime().UnixNano(), Inode: inode(fi)}
	if c, info, ok, err := a.n.Store.ImportCacheGet(src, a.hash); err != nil {
		return cid.CID{}, err
	} else if ok && (!info.NoCopy || a.req.NoCopy) {
		a.progress(fi.Size(), false)
		return c, nil
	}
	if a.hash == cid.SHA256 && fi.Size() > cid.MaxWholeObject {
		return cid.CID{}, errTooLargeSHA256(path, fi.Size())
	}
	var c cid.CID
	var err error
	if a.req.NoCopy {
		if err := a.o.Context().Err(); err != nil {
			return cid.CID{}, err
		}
		c, err = a.n.Store.AddNoCopy(path)
		a.progress(fi.Size(), false)
	} else {
		c, err = a.n.Store.AddBlobHash(reader{a, f}, fi.Size(), a.hash)
	}
	if err != nil {
		if errors.Is(err, cid.ErrTooLarge) {
			return cid.CID{}, errTooLargeSHA256(path, fi.Size())
		}
		return cid.CID{}, err
	}
	if err := a.n.Store.ImportCachePut(src, a.hash, c); err != nil {
		return cid.CID{}, err
	}
	return c, nil
}

func (a *adder) pin(c cid.CID) error {
	if !a.req.Pin {
		return nil
	}
	a.o.Stage("pinning")
	return a.n.Store.Pin(pinFor(c, ""))
}

func (a *adder) file(fi os.FileInfo) (*apiv0.AddResult, error) {
	a.total = fi.Size()
	a.o.Stage("adding")
	f, err := os.Open(a.req.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c, err := a.addFile(a.req.Path, f, fi)
	if err != nil {
		return nil, err
	}
	a.progress(0, true)
	name := filepath.Base(a.req.Path)
	root := c
	if !a.req.Raw {
		node := tree.FileNode{Src: c, Nu: tree.FileMeta{Size: uint64(fi.Size()), Name: name}}
		if fi.Mode().Perm()&0o100 != 0 {
			node.Nu.Mode = tree.ModeExec
		}
		hc, data, err := drisl.Sum(node)
		if err != nil {
			return nil, err
		}
		if err := a.n.Store.Put(hc, data); err != nil {
			return nil, err
		}
		root = hc
	}
	if err := a.pin(root); err != nil {
		return nil, err
	}
	return &apiv0.AddResult{CID: root.String(), Name: name, Size: fi.Size(), Files: 1}, nil
}

func (a *adder) dir() (*apiv0.AddResult, error) {
	root := a.req.Path
	a.o.Stage("scanning")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := a.o.Context().Err(); cerr != nil {
			return cerr
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				a.total += fi.Size()
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	a.o.Stage("adding")
	a.o.Progress(0, a.total, "bytes")
	res, err := tree.BuildFS(os.DirFS(root), ".", tree.BuildOptions{
		Blobs: a,
		Nodes: a.n.Store,
		AddFile: func(p string, f fs.File, fi fs.FileInfo) (cid.CID, error) {
			return a.addFile(filepath.Join(root, filepath.FromSlash(p)), f, fi)
		},
	})
	if err != nil {
		if errors.Is(err, cid.ErrTooLarge) {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err).WithAction("add it with the default BLAKE3 hash, which has no size limit")
		}
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "%v", err).WithAction("check that every file under " + root + " is readable, then add again")
		}
		return nil, err
	}
	a.progress(0, true)
	for _, s := range res.Skipped {
		a.o.Log(fmt.Sprintf("skipped %s: not a regular file, directory or symlink", filepath.Join(root, filepath.FromSlash(s))))
	}
	if err := a.pin(res.Header); err != nil {
		return nil, err
	}
	return &apiv0.AddResult{CID: res.Header.String(), Name: filepath.Base(root), Size: int64(res.Bytes), Files: int64(res.Files), Skipped: res.Skipped}, nil
}
