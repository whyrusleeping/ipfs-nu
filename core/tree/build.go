package tree

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"path"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
)

// BlobSink stores raw blobs for the builder (the store, or a hashing-only sink).
type BlobSink interface {
	// AddBlob consumes exactly size bytes from r and returns the blob's CID.
	AddBlob(r io.Reader, size int64) (cid.CID, error)
}

// BuildOptions configures BuildFS.
type BuildOptions struct {
	Blobs BlobSink
	Nodes mst.Store // receives directory MST nodes and the header
	// AddFile, when set, stores regular files instead of Blobs.AddBlob: it receives the
	// file's path within fsys, the open file and its FileInfo, and returns the blob CID. A
	// node uses it for --nocopy references and its import cache.
	AddFile func(path string, f fs.File, fi fs.FileInfo) (cid.CID, error)
}

// BuildResult describes a built tree.
type BuildResult struct {
	Header  cid.CID // the io.ipfs.nu.tree header
	Root    cid.CID // root directory MST
	Files   uint64
	Bytes   uint64
	Skipped []string // paths of unsupported file types (devices, sockets, FIFOs)
}

// BuildFS walks directory dir of fsys into blobs and a tree. It reads fsys only through
// the fs interfaces (os.DirFS in the daemon), so it does no I/O of its own. Symlinks are
// recorded, not followed, when fsys implements fs.ReadLinkFS; otherwise they are skipped.
func BuildFS(fsys fs.FS, dir string, o BuildOptions) (BuildResult, error) {
	b := &builder{fsys: fsys, o: o}
	root, size, err := b.dir(dir)
	if err != nil {
		return BuildResult{}, err
	}
	h := Header{V: 1, Root: root, Files: b.files, Bytes: size}
	hc, data, err := drisl.Sum(h)
	if err != nil {
		return BuildResult{}, err
	}
	if err := o.Nodes.Put(hc, data); err != nil {
		return BuildResult{}, err
	}
	return BuildResult{Header: hc, Root: root, Files: b.files, Bytes: size, Skipped: b.skipped}, nil
}

type builder struct {
	fsys    fs.FS
	o       BuildOptions
	files   uint64
	skipped []string
}

func (b *builder) dir(p string) (cid.CID, uint64, error) {
	ents, err := fs.ReadDir(b.fsys, p)
	if err != nil {
		return cid.CID{}, 0, err
	}
	t := Dir(b.o.Nodes, cid.CID{})
	var total uint64
	for _, de := range ents {
		name := de.Name()
		if err := ValidName(name); err != nil {
			return cid.CID{}, 0, fmt.Errorf("%s: %w (rename or exclude it)", path.Join(p, name), err)
		}
		full := path.Join(p, name)
		var e Entry
		switch ft := de.Type(); {
		case ft.IsDir():
			c, size, err := b.dir(full)
			if err != nil {
				return cid.CID{}, 0, err
			}
			e = Entry{Kind: KindDir, CID: c, Size: size}
		case ft.IsRegular():
			if e, err = b.file(full); err != nil {
				return cid.CID{}, 0, err
			}
		case ft&fs.ModeSymlink != 0:
			target, err := fs.ReadLink(b.fsys, full)
			if err != nil {
				b.skipped = append(b.skipped, full)
				continue
			}
			c, err := b.o.Blobs.AddBlob(bytes.NewReader([]byte(target)), int64(len(target)))
			if err != nil {
				return cid.CID{}, 0, err
			}
			e = Entry{Kind: KindSymlink, CID: c, Size: uint64(len(target))}
		default:
			b.skipped = append(b.skipped, full)
			continue
		}
		total += e.Size
		if err := t.Put(name, e); err != nil {
			return cid.CID{}, 0, err
		}
	}
	c, err := t.Flush()
	if err != nil {
		return cid.CID{}, 0, fmt.Errorf("%s: %w", p, err)
	}
	return c, total, nil
}

func (b *builder) file(p string) (Entry, error) {
	f, err := b.fsys.Open(p)
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Entry{}, err
	}
	var c cid.CID
	if b.o.AddFile != nil {
		c, err = b.o.AddFile(p, f, st)
	} else {
		c, err = b.o.Blobs.AddBlob(f, st.Size())
	}
	if err != nil {
		return Entry{}, fmt.Errorf("%s: %w", p, err)
	}
	b.files++
	e := Entry{Kind: KindFile, CID: c, Size: uint64(st.Size())}
	if st.Mode().Perm()&0o100 != 0 {
		e.Mode = ModeExec
	}
	return e, nil
}

// HashSink is a BlobSink that only hashes (BLAKE3 by default), for computing CIDs without
// storing blobs.
type HashSink struct{ Hash cid.Hash }

func (h HashSink) AddBlob(r io.Reader, size int64) (cid.CID, error) {
	fn := h.Hash
	if fn == 0 {
		fn = cid.BLAKE3
	}
	if fn == cid.SHA256 && size > cid.MaxWholeObject {
		return cid.CID{}, cid.ErrTooLarge
	}
	hs := fn.New()
	n, err := io.Copy(hs, r)
	if err != nil {
		return cid.CID{}, err
	}
	if n != size {
		return cid.CID{}, fmt.Errorf("tree: blob changed size while reading (%d of %d bytes)", n, size)
	}
	return cid.New(cid.Raw, cid.FromHasher(fn, hs)), nil
}
