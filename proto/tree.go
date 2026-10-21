package proto

import (
	"fmt"
	"slices"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
)

// TreeID is the tree sync protocol (layer 2, docs/data.md D14).
const TreeID = "/nu/tree/1"

// Limits of /nu/tree/1.
const (
	// MaxInline is the largest blob sent inline: one chunk group.
	MaxInline = blob.GroupSize
	// MaxTreeHave bounds the have roots of one request.
	MaxTreeHave = 16
	// MaxTreeNodes bounds the CIDs of a nodes (batch) request.
	MaxTreeNodes = 256
	// MaxTreeFrame bounds one response frame: a tag byte and a structure node.
	MaxTreeFrame = 1 + drisl.MaxNodeSize
)

// Scope selects how much of the target a /nu/tree/1 request returns.
type Scope string

const (
	// ScopeNode returns the path chain and the target's own node (a directory's MST root).
	ScopeNode Scope = "node"
	// ScopeEntry returns the target entry: for a directory every node of its MST (a listing,
	// without descending into subdirectories), entries' meta records and inline blobs; for
	// a file its inline blob and meta record.
	ScopeEntry Scope = "entry"
	// ScopeSubtree returns everything below the target.
	ScopeSubtree Scope = "subtree"
)

// ProfileNuTree is the walk profile of Nu trees and the request default.
const ProfileNuTree = "nu-tree"

// Link names how the walk reached an item (see Cursor).
type Link uint8

const (
	LinkRoot   Link = 0 // the request root itself
	LinkDir    Link = 1 // root MST node of the directory at Path (from a header or an entry)
	LinkLeft   Link = 2 // left subtree of the MST node whose first entry is Key
	LinkTarget Link = 3 // blob of the file or symlink entry Key
	LinkMeta   Link = 4 // meta record of entry Key
	LinkRight  Link = 5 // right subtree of entry Key
	LinkSrc    Link = 6 // blob of a MASL file-node root
	maxLink         = LinkSrc
)

// Cursor is a walk position: the location of the last item a client handled. Path is the
// directory (from the root directory) holding the link, Key the entry it hangs off. A
// response resumes after that item: with its descendants next, or after its whole subtree
// when After is set (the item was a blob, a marker, or not received). Every location is
// unique within a tree, so a cursor is valid at any peer holding the tree.
type Cursor struct {
	Path  []string
	Key   string
	Link  Link
	After bool
}

// NameRange is a half-open range [Start, End) of entry names in the target directory. An
// empty bound is open.
type NameRange struct {
	Start, End string
}

func (r *NameRange) has(k string) bool {
	return r == nil || (k >= r.Start && (r.End == "" || k < r.End))
}

// TreeRequest is a /nu/tree/1 request. Either Root (with the walk options) or Nodes (a
// batch of nodes by CID, Scope ScopeNode) is set.
type TreeRequest struct {
	Root      cid.CID
	Profile   string // walk profile; "" means nu-tree
	Path      []string
	NameRange *NameRange
	Limit     uint64 // most data frames in the response; 0 means the server's budget
	Scope     Scope
	Have      []cid.CID // complete trees the client holds (tree headers or directory roots)
	InlineMax uint64    // inline blobs of at most this size (capped at MaxInline)
	Cursor    *Cursor
	Nodes     []cid.CID
}

type treeRequestWire struct {
	Root      cid.CID     `drisl:"root,omitempty"`
	Profile   string      `drisl:"profile,omitempty"`
	Path      []string    `drisl:"path,omitempty"`
	NameRange []string    `drisl:"name_range,omitempty"`
	Limit     uint64      `drisl:"limit,omitempty"`
	Scope     string      `drisl:"scope"`
	Have      []cid.CID   `drisl:"have,omitempty"`
	InlineMax uint64      `drisl:"inline_max,omitempty"`
	Cursor    *cursorWire `drisl:"cursor,omitempty"`
	Nodes     []cid.CID   `drisl:"nodes,omitempty"`
}

type cursorWire struct {
	Path  []string `drisl:"path,omitempty"`
	Key   string   `drisl:"key,omitempty"`
	Link  uint64   `drisl:"link"`
	After bool     `drisl:"after,omitempty"`
}

// inline is the effective inline limit.
func (r *TreeRequest) inline() uint64 { return min(r.InlineMax, MaxInline) }

func (r *TreeRequest) check() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{errRequest}, a...)...)
	}
	if len(r.Nodes) > 0 {
		if len(r.Nodes) > MaxTreeNodes {
			return bad("more than %d nodes", MaxTreeNodes)
		}
		if r.Root.Defined() || r.Profile != "" || len(r.Path) > 0 || r.NameRange != nil || len(r.Have) > 0 || r.Cursor != nil || r.Scope != ScopeNode {
			return bad("a nodes request takes only nodes, scope node, limit and inline_max")
		}
		for _, c := range r.Nodes {
			if !c.Defined() {
				return bad("undefined node CID")
			}
		}
		return nil
	}
	if !r.Root.Defined() || r.Root.Codec() != cid.DRISL {
		return bad("root must be a DRISL node")
	}
	if r.Profile != "" && r.Profile != ProfileNuTree {
		return bad("unsupported walk profile %q", r.Profile)
	}
	switch r.Scope {
	case ScopeNode, ScopeEntry, ScopeSubtree:
	default:
		return bad("unknown scope %q", r.Scope)
	}
	for _, n := range r.Path {
		if err := tree.ValidName(n); err != nil {
			return bad("path: %v", err)
		}
	}
	if nr := r.NameRange; nr != nil {
		if r.Scope == ScopeNode || (nr.End != "" && nr.End <= nr.Start) {
			return bad("name_range needs scope entry or subtree and a non-empty range")
		}
	}
	if len(r.Have) > MaxTreeHave {
		return bad("more than %d have roots", MaxTreeHave)
	}
	if c := r.Cursor; c != nil {
		keyless := c.Link == LinkRoot || c.Link == LinkDir || c.Link == LinkSrc
		if c.Link > maxLink || keyless != (c.Key == "") || ((c.Link == LinkRoot || c.Link == LinkSrc) && len(c.Path) > 0) {
			return bad("invalid cursor")
		}
		for _, n := range append(slices.Clip(c.Path), c.Key) {
			if n != "" && tree.ValidName(n) != nil {
				return bad("invalid cursor")
			}
		}
	}
	return nil
}

// Marshal encodes the request as DRISL.
func (r TreeRequest) Marshal() ([]byte, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	w := treeRequestWire{Root: r.Root, Profile: r.Profile, Path: r.Path, Limit: r.Limit, Scope: string(r.Scope),
		Have: r.Have, InlineMax: r.InlineMax, Nodes: r.Nodes}
	if r.NameRange != nil {
		w.NameRange = []string{r.NameRange.Start, r.NameRange.End}
	}
	if c := r.Cursor; c != nil {
		w.Cursor = &cursorWire{Path: c.Path, Key: c.Key, Link: uint64(c.Link), After: c.After}
	}
	b, err := drisl.Marshal(w)
	if err == nil && len(b) > MaxRequestSize {
		err = fmt.Errorf("%w: request over %d bytes", errRequest, MaxRequestSize)
	}
	return b, err
}

// ParseTreeRequest decodes and validates a request. Unknown keys are ignored.
func ParseTreeRequest(b []byte) (TreeRequest, error) {
	var w treeRequestWire
	if err := (drisl.DecOptions{MaxSize: MaxRequestSize, MaxDepth: 8}).Unmarshal(b, &w); err != nil {
		return TreeRequest{}, fmt.Errorf("%w: %w", errRequest, err)
	}
	r := TreeRequest{Root: w.Root, Profile: w.Profile, Path: w.Path, Limit: w.Limit, Scope: Scope(w.Scope),
		Have: w.Have, InlineMax: w.InlineMax, Nodes: w.Nodes}
	if w.NameRange != nil {
		if len(w.NameRange) != 2 {
			return TreeRequest{}, fmt.Errorf("%w: name_range is [start, end]", errRequest)
		}
		r.NameRange = &NameRange{Start: w.NameRange[0], End: w.NameRange[1]}
	}
	if c := w.Cursor; c != nil {
		if c.Link > uint64(maxLink) {
			return TreeRequest{}, fmt.Errorf("%w: invalid cursor", errRequest)
		}
		r.Cursor = &Cursor{Path: c.Path, Key: c.Key, Link: Link(c.Link), After: c.After}
	}
	return r, r.check()
}

// Response frames after the {status, error?} header: <uvarint len> <tag> <payload>.
const (
	frameEnd     = 0 // payload DRISL {more?: true}; always last
	frameData    = 1 // payload is the next item's bytes (node or inline blob)
	frameHave    = 2 // the next item is in one of the request's have trees; not descended
	frameMissing = 3 // the server does not hold the next item; not descended
)

type treeTrailer struct {
	More bool `drisl:"more,omitempty"`
}
