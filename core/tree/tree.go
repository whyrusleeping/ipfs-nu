// Package tree holds Nu's tree formats (D3, D3a): directory entries in the Nu directory MST
// profile, the `io.ipfs.nu.tree` header, single-file MASL nodes, the nu-tree walk profile,
// and a builder that turns a directory (any fs.FS) into blobs and structure.
package tree

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
)

// Entry kinds.
const (
	KindFile    = "file"
	KindDir     = "dir"
	KindSymlink = "symlink"
)

const (
	// ModeExec is the only recorded file mode: files with the owner-executable bit set.
	ModeExec = 0o755
	// MaxNameLen bounds entry names in bytes.
	MaxNameLen = 255
	// MaxMetaSize bounds `meta` records (D3).
	MaxMetaSize = 64 << 10
)

// Entry is a directory entry: { kind, cid, size, mode?, mime?, meta? }.
//   - file: cid is the raw blob (BLAKE3 or SHA-256), size its length; mode is 0o755 for
//     executables and omitted otherwise.
//   - dir: cid is the directory's MST root node; size is the sum of its entries' sizes.
//   - symlink: cid is a raw blob holding the link target bytes; size is their length.
type Entry struct {
	Kind string  `drisl:"kind"`
	CID  cid.CID `drisl:"cid"`
	Size uint64  `drisl:"size"`
	Mode uint32  `drisl:"mode,omitempty"`
	Mime string  `drisl:"mime,omitempty"`
	Meta cid.CID `drisl:"meta,omitempty"`
}

// Validate checks an entry's kind/CID combination.
func (e Entry) Validate() error {
	switch e.Kind {
	case KindFile, KindSymlink:
		if !e.CID.Defined() || e.CID.Codec() != cid.Raw {
			return fmt.Errorf("tree: %s entry must link a raw blob", e.Kind)
		}
	case KindDir:
		if !e.CID.Defined() || e.CID.Codec() != cid.DRISL || e.CID.Hash() != cid.SHA256 {
			return errors.New("tree: dir entry must link a SHA-256 DRISL node")
		}
	default:
		return fmt.Errorf("tree: unknown entry kind %q", e.Kind)
	}
	if e.Mode != 0 && (e.Kind != KindFile || e.Mode != ModeExec) {
		return fmt.Errorf("tree: mode %o not allowed on a %s entry", e.Mode, e.Kind)
	}
	if e.Meta.Defined() && (e.Meta.Codec() != cid.DRISL || e.Meta.Hash() != cid.SHA256) {
		return errors.New("tree: meta must link a SHA-256 DRISL record")
	}
	return nil
}

// ValidName enforces the entry-name rules: non-empty valid UTF-8 of at most 255 bytes,
// without '/' or NUL, and not "." or "..". Names are kept byte-exact (no normalization).
func ValidName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return fmt.Errorf("tree: invalid name %q", name)
	case len(name) > MaxNameLen:
		return fmt.Errorf("tree: name longer than %d bytes", MaxNameLen)
	case !utf8.ValidString(name):
		return fmt.Errorf("tree: name %q is not valid UTF-8", name)
	case strings.ContainsAny(name, "/\x00"):
		return fmt.Errorf("tree: name %q contains '/' or NUL", name)
	}
	return nil
}

type dirCodec struct{ mst.InlineCodec[Entry] }

func (c dirCodec) Decode(data []byte) (*mst.Node[Entry], error) {
	n, err := c.InlineCodec.Decode(data)
	if err != nil {
		return nil, err
	}
	for _, e := range n.Entries {
		if err := e.Val.Validate(); err != nil {
			return nil, err
		}
	}
	return n, nil
}

// DirProfile is the Nu directory MST profile: SHA-256 key depth in 4-bit units (fanout 16),
// inline entries, 1 MiB nodes.
var DirProfile = &mst.Profile[Entry]{
	Name:        "nu-dir",
	LevelBits:   4,
	MaxNodeSize: drisl.MaxNodeSize,
	Codec:       dirCodec{},
	ValidKey:    ValidName,
	Equal:       func(a, b Entry) bool { return a == b },
}

// Dir opens the directory whose MST root is c (zero for a new empty directory).
func Dir(st mst.Store, c cid.CID) *mst.Tree[Entry] { return DirProfile.Load(st, c) }

// Header is the tree header record.
type Header struct {
	V          int64          `drisl:"v"`
	Root       cid.CID        `drisl:"root"`
	Files      uint64         `drisl:"files"` // file entries in the whole tree
	Bytes      uint64         `drisl:"bytes"` // the root directory's size
	Collection map[string]any `drisl:"collection,omitempty"`
}

// TypeHeader is the header's $type.
const TypeHeader = "io.ipfs.nu.tree"

func (Header) DRISLType() string { return TypeHeader }

// DecodeHeader decodes and checks a header (version 1).
func DecodeHeader(data []byte) (Header, error) {
	var h Header
	if err := drisl.Unmarshal(data, &h); err != nil {
		return h, err
	}
	if h.V != 1 {
		return h, fmt.Errorf("tree: unsupported header version %d", h.V)
	}
	if !h.Root.Defined() || h.Root.Codec() != cid.DRISL || h.Root.Hash() != cid.SHA256 {
		return h, errors.New("tree: header root must be a SHA-256 DRISL node")
	}
	return h, nil
}

// FileNode is a single-mode MASL document wrapping one blob (D3a).
type FileNode struct {
	Src         cid.CID  `drisl:"src"`
	ContentType string   `drisl:"content-type,omitempty"`
	Nu          FileMeta `drisl:"io.ipfs.nu"`
}

// FileMeta is the `io.ipfs.nu` member of a file node.
type FileMeta struct {
	Size uint64 `drisl:"size"`
	Name string `drisl:"name,omitempty"`
	Mode uint32 `drisl:"mode,omitempty"`
}

// TypeMASL is the MASL $type.
const TypeMASL = "ing.dasl.masl"

func (FileNode) DRISLType() string { return TypeMASL }

// DecodeFileNode decodes a single-mode MASL file node.
func DecodeFileNode(data []byte) (FileNode, error) {
	var f FileNode
	if err := drisl.Unmarshal(data, &f); err != nil {
		return f, err
	}
	if !f.Src.Defined() || f.Src.Codec() != cid.Raw {
		return f, errors.New("tree: file node src must be a raw blob")
	}
	return f, nil
}

// ErrNotFound is returned by Resolve for missing paths.
var ErrNotFound = errors.New("tree: no such path")

// Resolve follows a slash-separated path from directory root, touching only the MST search
// path in each directory. The empty path resolves to the root itself as a dir entry with
// unknown size.
func Resolve(st mst.Store, root cid.CID, p string) (Entry, error) {
	cur := Entry{Kind: KindDir, CID: root}
	p = strings.Trim(path.Clean("/"+p), "/")
	if p == "" {
		return cur, nil
	}
	for _, name := range strings.Split(p, "/") {
		if cur.Kind != KindDir {
			return Entry{}, fmt.Errorf("%w: %q is not a directory", ErrNotFound, name)
		}
		e, ok, err := Dir(st, cur.CID).Get(name)
		if err != nil {
			return Entry{}, err
		}
		if !ok {
			return Entry{}, fmt.Errorf("%w: %s", ErrNotFound, p)
		}
		cur = e
	}
	return cur, nil
}

// Links is the nu-tree walk profile's link function (a car.LinksFunc): header → root
// directory; directory MST node → in key order the left subtree, then per entry its target
// (blob or directory), its meta record and its right subtree; file node → src. Raw blocks
// and other DRISL have no links in this profile.
func Links(c cid.CID, data []byte) ([]cid.CID, error) {
	if c.Codec() != cid.DRISL {
		return nil, nil
	}
	v, err := drisl.Decode(data)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil
	}
	switch m["$type"] {
	case TypeHeader:
		h, err := DecodeHeader(data)
		if err != nil {
			return nil, err
		}
		return []cid.CID{h.Root}, nil
	case TypeMASL:
		if s, ok := m["src"].(cid.CID); ok {
			return []cid.CID{s}, nil
		}
		var out []cid.CID // bundle mode: every resource src
		if res, ok := m["resources"].(map[string]any); ok {
			drisl.Links(res, func(l cid.CID) { out = append(out, l) })
		}
		return out, nil
	}
	if _, ok := m["e"]; !ok {
		return nil, nil
	}
	n, err := DirProfile.Codec.Decode(data)
	if err != nil {
		return nil, err
	}
	var out []cid.CID
	if n.Left.Defined() {
		out = append(out, n.Left)
	}
	for _, e := range n.Entries {
		out = append(out, e.Val.CID)
		if e.Val.Meta.Defined() {
			out = append(out, e.Val.Meta)
		}
		if e.Right.Defined() {
			out = append(out, e.Right)
		}
	}
	return out, nil
}
