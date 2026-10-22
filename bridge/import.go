package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/spaolacci/murmur3"
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
	OnBlock                 func(int, IPFSCID)
}
type FileMapping struct {
	Path   string   `drisl:"path"`
	Source []string `drisl:"source"`
	Blob   cid.CID  `drisl:"blob"`
	Kind   string   `drisl:"kind"`
}
type ImportResult struct {
	IPFSRoot string        `drisl:"ipfs_root" json:"ipfs_root"`
	Root     cid.CID       `drisl:"nu_root" json:"root"`
	Manifest cid.CID       `drisl:"manifest" json:"manifest"`
	Files    uint64        `drisl:"files" json:"files"`
	Bytes    uint64        `drisl:"bytes" json:"bytes"`
	Mappings []FileMapping `drisl:"files_map" json:"mappings,omitempty"`
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
	var nuRoot cid.CID
	var data []byte
	if e.Kind == tree.KindDir {
		header := tree.Header{V: 1, Root: e.CID, Files: i.files, Bytes: e.Size}
		nuRoot, data, err = drisl.Sum(header)
		if err != nil {
			return ImportResult{}, err
		}
		if err = nodes.Put(nuRoot, data); err != nil {
			return ImportResult{}, err
		}
	} else if e.Kind == tree.KindFile {
		// A standalone UnixFS file becomes the standard Nu file node (D3a), not an
		// artificial one-entry directory with a fabricated filename.
		file := tree.FileNode{Src: e.CID, Nu: tree.FileMeta{Size: e.Size}}
		nuRoot, data, err = drisl.Sum(file)
		if err != nil {
			return ImportResult{}, err
		}
		if err = nodes.Put(nuRoot, data); err != nil {
			return ImportResult{}, err
		}
	} else {
		return ImportResult{}, errors.New("bridge: a standalone UnixFS symlink has no directory entry name; import its containing directory")
	}
	result := ImportResult{IPFSRoot: root.String(), Root: nuRoot, Files: i.files, Bytes: i.total, Mappings: i.mappings}
	manifest := map[string]any{"$type": "io.ipfs.nu.import", "v": int64(1), "ipfs_root": root.String(), "nu_root": nuRoot, "importer": opts.ImporterVersion, "source": opts.Source, "verified": true, "files": i.files, "bytes": i.total, "mappings": i.mappings}
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
	if i.opts.OnBlock != nil {
		i.opts.OnBlock(i.blocks, c)
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
	if u.HasFilesize && u.Filesize > math.MaxInt64 {
		return tree.Entry{}, errors.New("bridge: UnixFS filesize exceeds local store bounds")
	}
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
	// AddBlob consumes the declared filesize. Close the read side before awaiting the
	// producer so a false undersized claim cannot strand its streaming writer forever.
	_ = pr.Close()
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
	var n int64
	cw := countWriter{w: w, n: &n}
	if len(u.Data) > 0 {
		if _, e = cw.Write(u.Data); e != nil {
			return e
		}
	}
	if len(u.Blocksizes) > 0 && len(u.Blocksizes) != len(pb.Links) {
		return errors.New("bridge: nested UnixFS blocksizes count does not match links")
	}
	for idx, l := range pb.Links {
		x, e := ParseIPFSBytes(l.CID)
		if e != nil {
			return e
		}
		before := n
		if e = i.filePart(x, &cw, depth+1); e != nil {
			return e
		}
		if len(u.Blocksizes) > 0 && uint64(n-before) != u.Blocksizes[idx] {
			return fmt.Errorf("bridge: nested UnixFS blocksize mismatch for link %d", idx)
		}
	}
	if u.HasFilesize && uint64(n) != u.Filesize {
		return fmt.Errorf("bridge: nested UnixFS filesize mismatch: have %d want %d", n, u.Filesize)
	}
	return nil
}
func (i *importer) hamtLinks(c IPFSCID, u UnixFSData, links []Link, depth int) ([]Link, error) {
	return i.hamtLinksAt(c, u, links, depth, 0, u.Fanout, nil)
}

func (i *importer) hamtLinksAt(c IPFSCID, u UnixFSData, links []Link, depth, consumed int, fanout uint64, parentSlots []uint64) ([]Link, error) {
	if depth > i.opts.MaxDepth || fanout == 0 || fanout > 1<<10 || fanout&(fanout-1) != 0 || u.Fanout != fanout || u.HashType != 0x22 {
		return nil, errors.New("bridge: unsupported HAMT fanout/hash type or nesting depth")
	}
	bitsPerLevel := 0
	for v := fanout; v > 1; v >>= 1 {
		bitsPerLevel++
	}
	digits := (bitsPerLevel + 3) / 4
	if len(u.Data) == 0 || len(u.Data) > int(fanout/8) {
		return nil, fmt.Errorf("bridge: HAMT bitfield has invalid size: got %d bytes for fanout %d", len(u.Data), fanout)
	}
	seen := make(map[uint64]bool, len(links))
	setBits := 0
	for _, b := range u.Data {
		for x := b; x != 0; x &= x - 1 {
			setBits++
		}
	}
	if setBits != len(links) {
		return nil, errors.New("bridge: HAMT bitfield/link count mismatch")
	}
	var out []Link
	var prev uint64
	for n, l := range links {
		if len(l.Name) < digits {
			return nil, errors.New("bridge: invalid HAMT link prefix/name")
		}
		prefix := l.Name[:digits]
		for _, r := range prefix {
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return nil, errors.New("bridge: malformed HAMT prefix")
			}
		}
		slot, e := strconv.ParseUint(prefix, 16, 64)
		if e != nil || slot >= fanout {
			return nil, errors.New("bridge: HAMT prefix index out of range")
		}
		if seen[slot] || (n > 0 && slot <= prev) {
			return nil, errors.New("bridge: duplicate or unordered HAMT link prefix")
		}
		seen[slot] = true
		prev = slot
		if slot/8 >= uint64(len(u.Data)) || u.Data[len(u.Data)-1-int(slot/8)]&(1<<(slot%8)) == 0 {
			return nil, errors.New("bridge: HAMT link absent from bitfield")
		}
		name := l.Name[digits:]
		if name != "" {
			hash := murmur3.Sum64([]byte(name))
			slots := append(append([]uint64(nil), parentSlots...), slot)
			for level, want := range slots {
				shift := uint(64 - (level+1)*bitsPerLevel)
				got := (hash >> shift) & (fanout - 1)
				if got != want {
					return nil, fmt.Errorf("bridge: HAMT name %q hashes to shard %x, link uses %x", name, got, want)
				}
			}
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
		nested, e := i.hamtLinksAt(child, ud, pb.Links, depth+1, consumed+bitsPerLevel, fanout, append(append([]uint64(nil), parentSlots...), slot))
		if e != nil {
			return nil, e
		}
		out = append(out, nested...)
	}
	return out, nil
}
