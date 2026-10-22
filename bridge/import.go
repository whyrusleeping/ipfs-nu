package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
)

// BlockSource supplies original IPFS blocks. Implementations must not treat source claims as
// verification: Import verifies each byte slice against its requested IPFSCID.
type BlockSource interface {
	GetBlock(context.Context, IPFSCID) ([]byte, error)
}
type BlobSink interface {
	AddBlob(io.Reader, int64) (cid.CID, error)
}

type ImportOptions struct {
	ImporterVersion, Source string
	MaxDepth, MaxBlocks     int
}
type FileMapping struct {
	Path   string   `drisl:"path"`
	Source []string `drisl:"source"`
	Blob   cid.CID  `drisl:"blob"`
	Kind   string   `drisl:"kind"`
}
type ImportResult struct {
	IPFSRoot string        `drisl:"ipfs_root"`
	Root     cid.CID       `drisl:"nu_root"`
	Manifest cid.CID       `drisl:"manifest"`
	Files    uint64        `drisl:"files"`
	Bytes    uint64        `drisl:"bytes"`
	Mappings []FileMapping `drisl:"files_map"`
}
type importer struct {
	ctx          context.Context
	src          BlockSource
	blobs        BlobSink
	nodes        mst.Store
	opts         ImportOptions
	seen         map[IPFSCID]bool
	blocks       int
	mappings     []FileMapping
	files, total uint64
}

// Import verifies and converts one UnixFS root into Nu's D3 tree and stores a canonical,
// content-addressed DRISL provenance manifest. Nodes is the Nu structure store; Blobs is
// normally store.Store. No source block is copied into the Nu tree under its original CID.
func Import(ctx context.Context, root IPFSCID, src BlockSource, blobs BlobSink, nodes mst.Store, opts ImportOptions) (ImportResult, error) {
	if src == nil || blobs == nil || nodes == nil {
		return ImportResult{}, errors.New("bridge: source, blob sink and node store are required")
	}
	if opts.MaxDepth == 0 {
		opts.MaxDepth = 1024
	}
	if opts.MaxBlocks == 0 {
		opts.MaxBlocks = 1 << 20
	}
	if opts.ImporterVersion == "" {
		opts.ImporterVersion = "dev"
	}
	i := &importer{ctx: ctx, src: src, blobs: blobs, nodes: nodes, opts: opts, seen: map[IPFSCID]bool{}}
	e, err := i.node(root, "", 0)
	if err != nil {
		return ImportResult{}, err
	}
	if e.Kind != tree.KindDir {
		return ImportResult{}, errors.New("bridge: root must be a UnixFS directory")
	}
	header := tree.Header{V: 1, Root: e.CID, Files: i.files, Bytes: e.Size}
	hc, hdata, err := drisl.Sum(header)
	if err != nil {
		return ImportResult{}, err
	}
	if err = nodes.Put(hc, hdata); err != nil {
		return ImportResult{}, err
	}
	result := ImportResult{IPFSRoot: root.String(), Root: hc, Files: i.files, Bytes: i.total, Mappings: i.mappings}
	manifest := map[string]any{"$type": "io.ipfs.nu.import", "v": int64(1), "ipfs_root": root.String(), "nu_root": hc, "importer": opts.ImporterVersion, "source": opts.Source, "verified": true, "files": i.files, "bytes": i.total, "mappings": i.mappings}
	mc, mb, err := drisl.Sum(manifest)
	if err != nil {
		return ImportResult{}, err
	}
	if err = nodes.Put(mc, mb); err != nil {
		return ImportResult{}, err
	}
	result.Manifest = mc
	return result, nil
}

func (i *importer) block(c IPFSCID) ([]byte, error) {
	if i.blocks >= i.opts.MaxBlocks {
		return nil, errors.New("bridge: block limit exceeded")
	}
	i.blocks++
	b, e := i.src.GetBlock(i.ctx, c)
	if e != nil {
		return nil, e
	}
	if e = c.Verify(b); e != nil {
		return nil, fmt.Errorf("bridge: verify %s: %w", c.String(), e)
	}
	return b, nil
}
func (i *importer) node(c IPFSCID, p string, depth int) (tree.Entry, error) {
	if depth > i.opts.MaxDepth {
		return tree.Entry{}, errors.New("bridge: UnixFS depth limit exceeded")
	}
	if i.seen[c] {
		return tree.Entry{}, fmt.Errorf("bridge: cyclic or repeated UnixFS node %s", c.String())
	}
	i.seen[c] = true
	defer delete(i.seen, c)
	b, e := i.block(c)
	if e != nil {
		return tree.Entry{}, e
	}
	if c.Codec == codecRaw {
		return i.rawFile(c, p, b)
	}
	pb, e := DecodePBNode(b)
	if e != nil {
		return tree.Entry{}, e
	}
	u, e := DecodeUnixFSData(pb.Data)
	if e != nil {
		return tree.Entry{}, e
	}
	switch u.Type {
	case UnixFSFile:
		return i.file(c, p, u, pb.Links)
	case UnixFSSymlink:
		blob, e := i.blobs.AddBlob(bytes.NewReader(u.Data), int64(len(u.Data)))
		if e != nil {
			return tree.Entry{}, e
		}
		i.mappings = append(i.mappings, FileMapping{Path: p, Source: []string{c.String()}, Blob: blob, Kind: tree.KindSymlink})
		return tree.Entry{Kind: tree.KindSymlink, CID: blob, Size: uint64(len(u.Data))}, nil
	case UnixFSDirectory, UnixFSHAMTShard:
		entries := pb.Links
		if u.Type == UnixFSHAMTShard {
			entries, e = i.hamtLinks(c, u, pb.Links, depth)
			if e != nil {
				return tree.Entry{}, e
			}
		}
		t := tree.Dir(i.nodes, cid.CID{})
		var size uint64
		for _, l := range entries {
			if e := tree.ValidName(l.Name); e != nil {
				return tree.Entry{}, fmt.Errorf("bridge: unsafe IPFS name %q: %w", l.Name, e)
			}
			child, e := ParseIPFSBytes(l.CID)
			if e != nil {
				return tree.Entry{}, e
			}
			path := l.Name
			if p != "" {
				path = p + "/" + path
			}
			ent, e := i.node(child, path, depth+1)
			if e != nil {
				return tree.Entry{}, e
			}
			if e = t.Put(l.Name, ent); e != nil {
				return tree.Entry{}, e
			}
			size += ent.Size
		}
		root, e := t.Flush()
		if e != nil {
			return tree.Entry{}, e
		}
		return tree.Entry{Kind: tree.KindDir, CID: root, Size: size}, nil
	default:
		return tree.Entry{}, fmt.Errorf("bridge: unsupported UnixFS type %d", u.Type)
	}
}
func (i *importer) rawFile(c IPFSCID, p string, b []byte) (tree.Entry, error) {
	blob, e := i.blobs.AddBlob(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		return tree.Entry{}, e
	}
	i.files++
	i.total += uint64(len(b))
	i.mappings = append(i.mappings, FileMapping{Path: p, Source: []string{c.String()}, Blob: blob, Kind: tree.KindFile})
	return tree.Entry{Kind: tree.KindFile, CID: blob, Size: uint64(len(b))}, nil
}
func (i *importer) file(root IPFSCID, p string, u UnixFSData, links []Link) (tree.Entry, error) {
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	var n int64
	go func() {
		var e error
		w := countWriter{w: pw, n: &n}
		if len(u.Data) > 0 {
			_, e = w.Write(u.Data)
		}
		if len(u.Blocksizes) > 0 && len(u.Blocksizes) != len(links) {
			e = errors.New("bridge: UnixFS blocksizes count does not match links")
		}
		for idx, l := range links {
			if e != nil {
				break
			}
			c, x := ParseIPFSBytes(l.CID)
			if x != nil {
				e = x
				break
			}
			before := n
			x = i.filePart(c, &w, 1)
			if x != nil {
				e = x
				break
			}
			if len(u.Blocksizes) > 0 && uint64(n-before) != u.Blocksizes[idx] {
				e = fmt.Errorf("bridge: UnixFS blocksize mismatch for link %d", idx)
				break
			}
		}
		if e == nil && u.HasFilesize && uint64(n) != u.Filesize {
			e = fmt.Errorf("bridge: UnixFS filesize mismatch: have %d want %d", n, u.Filesize)
		}
		_ = pw.CloseWithError(e)
		errCh <- e
	}()
	blob, e := i.blobs.AddBlob(pr, func() int64 {
		if u.HasFilesize {
			return int64(u.Filesize)
		}
		return -1
	}())
	if e != nil {
		_ = pr.CloseWithError(e)
		<-errCh
		return tree.Entry{}, e
	}
	if e = <-errCh; e != nil {
		return tree.Entry{}, e
	}
	i.files++
	i.total += uint64(n)
	i.mappings = append(i.mappings, FileMapping{Path: p, Source: []string{root.String()}, Blob: blob, Kind: tree.KindFile})
	return tree.Entry{Kind: tree.KindFile, CID: blob, Size: uint64(n)}, nil
}

type countWriter struct {
	w io.Writer
	n *int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, e := c.w.Write(p)
	*c.n += int64(n)
	return n, e
}
func (i *importer) filePart(c IPFSCID, w io.Writer, depth int) error {
	if depth > i.opts.MaxDepth {
		return errors.New("bridge: file link depth limit")
	}
	b, e := i.block(c)
	if e != nil {
		return e
	}
	if c.Codec == codecRaw {
		_, e = w.Write(b)
		return e
	}
	pb, e := DecodePBNode(b)
	if e != nil {
		return e
	}
	u, e := DecodeUnixFSData(pb.Data)
	if e != nil {
		return e
	}
	if u.Type != UnixFSFile {
		return fmt.Errorf("bridge: non-file UnixFS node in file links (%d)", u.Type)
	}
	if len(u.Data) > 0 {
		if _, e = w.Write(u.Data); e != nil {
			return e
		}
	}
	for _, l := range pb.Links {
		x, e := ParseIPFSBytes(l.CID)
		if e != nil {
			return e
		}
		if e = i.filePart(x, w, depth+1); e != nil {
			return e
		}
	}
	return nil
}
func (i *importer) hamtLinks(c IPFSCID, u UnixFSData, links []Link, depth int) ([]Link, error) {
	if u.Fanout == 0 || u.Fanout > 1<<16 || u.HashType != 0x22 {
		return nil, errors.New("bridge: unsupported HAMT fanout/hash type")
	}
	var out []Link
	digits := 1
	for x := u.Fanout - 1; x > 15; x >>= 4 {
		digits++
	}
	for _, l := range links {
		if len(l.Name) < digits {
			return nil, errors.New("bridge: invalid HAMT link prefix")
		}
		prefix := l.Name[:digits]
		for _, r := range prefix {
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return nil, errors.New("bridge: malformed HAMT prefix")
			}
		}
		name := l.Name[digits:]
		if name != "" {
			l.Name = name
			out = append(out, l)
			continue
		}
		child, e := ParseIPFSBytes(l.CID)
		if e != nil {
			return nil, e
		}
		b, e := i.block(child)
		if e != nil {
			return nil, e
		}
		pb, e := DecodePBNode(b)
		if e != nil {
			return nil, e
		}
		ud, e := DecodeUnixFSData(pb.Data)
		if e != nil {
			return nil, e
		}
		if ud.Type != UnixFSHAMTShard {
			return nil, errors.New("bridge: HAMT bucket link does not point to a shard")
		}
		nested, e := i.hamtLinks(child, ud, pb.Links, depth+1)
		if e != nil {
			return nil, e
		}
		out = append(out, nested...)
	}
	return out, nil
}
