package proto

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
)

// The nu-tree walk of /nu/tree/1. Server and client run the same deterministic walk: the
// server emits a frame per item, the client reads one per item and checks it against the
// CID its verified parent linked. Neither side needs to trust the other's idea of where
// the walk is, and the client's position after any frame is a valid resume cursor.

var (
	errCursor      = errors.New("proto: cursor is not on the walk")
	errInvalidTree = errors.New("proto: invalid tree node")
	errBudget      = errors.New("proto: response budget reached")
)

type itemKind uint8

const (
	kindNode itemKind = iota
	kindBlob
)

// loc is an item's location (the link that reached it).
type loc struct {
	path []string
	key  string
	link Link
}

func (l *loc) is(c *Cursor) bool {
	return l.link == c.Link && l.key == c.Key && slices.Equal(l.path, c.Path)
}

func (l *loc) cursor(after bool) *Cursor {
	return &Cursor{Path: slices.Clone(l.path), Key: l.key, Link: l.link, After: after}
}

func hasPrefix(p, prefix []string) bool {
	return len(p) >= len(prefix) && slices.Equal(p[:len(prefix)], prefix)
}

// kbounds limits an MST subtree: keys strictly inside (lo, hi), layers below `below`.
type kbounds struct {
	lo, hi       string
	hasLo, hasHi bool
	below        int
}

var topBounds = kbounds{below: math.MaxInt}

func (b kbounds) has(k string) bool { return (!b.hasLo || k > b.lo) && (!b.hasHi || k < b.hi) }

type dirMode uint8

const (
	modeNone dirMode = iota // only the directory's root node
	modePath                // descending toward the next path name
	modeList                // the target directory with scope entry
	modeFull                // the target directory with scope subtree, and everything below
	modeSkip                // not visited
)

// dirCtx is the walk state of one directory.
type dirCtx struct {
	path []string
	mode dirMode
	rng  *NameRange // the request's name range, on the target directory only
	have []haveDir  // server: the directories at the same path in the have trees
}

type haveDir struct {
	root cid.CID
	t    *mst.Tree[tree.Entry]
}

// treeWalk is one walk. get loads nodes already held: the server's store, or for a client
// the nodes it verified earlier (used to seek to a cursor). visit handles an item past the
// cursor and returns node data to descend into (nil to stop there). shared, on the server,
// reports whether the item is in a have tree.
type treeWalk struct {
	req   *TreeRequest
	get   func(cid.CID) ([]byte, error)
	visit func(l *loc, c cid.CID, k itemKind, shared func() bool) ([]byte, error)
	seek  *Cursor // non-nil until the walk passes the cursor
	seen  map[cid.Multihash]bool
	// server only: have-tree access
	hst   mst.Store
	cache map[cid.CID]*mst.Node[tree.Entry]
}

func newTreeWalk(req *TreeRequest, get func(cid.CID) ([]byte, error)) *treeWalk {
	return &treeWalk{req: req, get: get, seek: req.Cursor, seen: map[cid.Multihash]bool{}}
}

func (w *treeWalk) run() error {
	if len(w.req.Nodes) > 0 {
		for _, c := range w.req.Nodes {
			k := kindNode
			if c.Codec() == cid.Raw {
				k = kindBlob
			}
			if _, err := w.visit(nil, c, k, nil); err != nil {
				return err
			}
		}
		return nil
	}
	l := &loc{link: LinkRoot}
	if err := w.item(l, w.req.Root, kindNode, nil, func(*Cursor) bool { return true }, w.root); err != nil {
		return err
	}
	if w.seek != nil {
		return errCursor
	}
	return nil
}

// item handles one item of the walk: while seeking it skips items before the cursor and
// descends (without visiting) into the one containing it.
func (w *treeWalk) item(l *loc, c cid.CID, k itemKind, shared func() bool, contains func(*Cursor) bool, descend func([]byte) error) error {
	if s := w.seek; s != nil {
		in := l.is(s)
		if !in && (contains == nil || !contains(s)) {
			return nil
		}
		if in {
			w.seek = nil
			if s.After || k == kindBlob {
				return nil
			}
		}
		data, err := w.get(c)
		if err != nil {
			return fmt.Errorf("%w: %w", errCursor, err)
		}
		if err := c.Verify(data); err != nil {
			return err
		}
		if err := descend(data); err != nil {
			return err
		}
		if w.seek != nil {
			return errCursor
		}
		return nil
	}
	if w.seen[c.Multihash()] {
		return nil
	}
	w.seen[c.Multihash()] = true
	data, err := w.visit(l, c, k, shared)
	if err != nil || data == nil || k == kindBlob {
		return err
	}
	return descend(data)
}

const (
	rootDir = iota
	rootHeader
	rootFile
)

func classifyRoot(data []byte) (int, error) {
	v, err := drisl.Decode(data)
	if err != nil {
		return 0, err
	}
	m, _ := v.(map[string]any)
	switch {
	case m == nil:
	case m["$type"] == tree.TypeHeader:
		return rootHeader, nil
	case m["$type"] == tree.TypeMASL && m["src"] != nil:
		return rootFile, nil
	case m["$type"] == nil && m["e"] != nil:
		return rootDir, nil
	}
	return 0, errors.New("not a tree header, directory or file node")
}

// targetMode is the mode of the requested directory.
func (w *treeWalk) targetMode() dirMode {
	switch w.req.Scope {
	case ScopeEntry:
		return modeList
	case ScopeSubtree:
		return modeFull
	}
	return modeNone
}

func (w *treeWalk) root(data []byte) error {
	kind, err := classifyRoot(data)
	if err != nil {
		return fmt.Errorf("%w: root: %w", errInvalidTree, err)
	}
	d := &dirCtx{mode: modePath, have: w.haveRoots()}
	if len(w.req.Path) == 0 {
		d.mode, d.rng = w.targetMode(), w.req.NameRange
	}
	switch kind {
	case rootHeader:
		h, err := tree.DecodeHeader(data)
		if err != nil {
			return fmt.Errorf("%w: %w", errInvalidTree, err)
		}
		return w.dir(d, h.Root, func() bool {
			for _, a := range d.have {
				if a.root == h.Root {
					return true
				}
			}
			return false
		})
	case rootFile:
		fn, err := tree.DecodeFileNode(data)
		if err != nil {
			return fmt.Errorf("%w: %w", errInvalidTree, err)
		}
		if len(w.req.Path) > 0 || w.req.Scope == ScopeNode || fn.Nu.Size > w.req.inline() {
			return nil
		}
		return w.item(&loc{link: LinkSrc}, fn.Src, kindBlob, nil, nil, nil)
	}
	if d.mode == modeNone {
		return nil
	}
	return w.node(d, data, topBounds)
}

// dir visits the root node of directory d (reached from a header or an entry) and walks
// it per d.mode.
func (w *treeWalk) dir(d *dirCtx, c cid.CID, shared func() bool) error {
	l := &loc{path: d.path, link: LinkDir}
	contains := func(cur *Cursor) bool { return hasPrefix(cur.Path, d.path) }
	return w.item(l, c, kindNode, shared, contains, func(data []byte) error {
		if d.mode == modeNone {
			return nil
		}
		return w.node(d, data, topBounds)
	})
}

// decodeDirNode decodes a directory MST node and checks it against its bounds: names valid,
// sorted, of one layer below the parent's and inside the parent gap; only a directory root
// may be empty.
func decodeDirNode(data []byte, b kbounds) (*mst.Node[tree.Entry], int, error) {
	n, err := tree.DirProfile.Codec.Decode(data)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", errInvalidTree, err)
	}
	if len(n.Entries) == 0 {
		if b != topBounds || n.Left.Defined() {
			return nil, 0, fmt.Errorf("%w: empty node", errInvalidTree)
		}
		return n, 0, nil
	}
	layer := tree.DirProfile.Layer(n.Entries[0].Key)
	if layer >= b.below {
		return nil, 0, fmt.Errorf("%w: layer %d not below %d", errInvalidTree, layer, b.below)
	}
	for i, e := range n.Entries {
		if err := tree.ValidName(e.Key); err != nil {
			return nil, 0, fmt.Errorf("%w: %w", errInvalidTree, err)
		}
		if (i > 0 && e.Key <= n.Entries[i-1].Key) || !b.has(e.Key) || tree.DirProfile.Layer(e.Key) != layer {
			return nil, 0, fmt.Errorf("%w: entry %q out of order, range or layer", errInvalidTree, e.Key)
		}
	}
	return n, layer, nil
}

func gapBounds(b kbounds, layer int, n *mst.Node[tree.Entry], i int) kbounds {
	g := kbounds{lo: b.lo, hi: b.hi, hasLo: b.hasLo, hasHi: b.hasHi, below: layer}
	if i > 0 {
		g.lo, g.hasLo = n.Entries[i-1].Key, true
	}
	if i < len(n.Entries) {
		g.hi, g.hasHi = n.Entries[i].Key, true
	}
	return g
}

// wantGap reports whether the walk enters a gap subtree.
func (w *treeWalk) wantGap(d *dirCtx, g kbounds) bool {
	if d.mode == modePath {
		return g.has(w.req.Path[len(d.path)])
	}
	if r := d.rng; r != nil {
		return !(g.hasHi && g.hi <= r.Start) && !(g.hasLo && r.End != "" && g.lo >= r.End)
	}
	return true
}

func (w *treeWalk) wantEntry(d *dirCtx, k string) bool {
	if d.mode == modePath {
		return k == w.req.Path[len(d.path)]
	}
	return d.rng.has(k)
}

// node walks one MST node of directory d: the left subtree, then per entry its target, its
// meta record and its right subtree (the nu-tree pre-order).
func (w *treeWalk) node(d *dirCtx, data []byte, b kbounds) error {
	n, layer, err := decodeDirNode(data, b)
	if err != nil || len(n.Entries) == 0 {
		return err
	}
	for i := 0; i <= len(n.Entries); i++ {
		g, l := n.Left, &loc{path: d.path, key: n.Entries[0].Key, link: LinkLeft}
		if i > 0 {
			g, l = n.Entries[i-1].Right, &loc{path: d.path, key: n.Entries[i-1].Key, link: LinkRight}
		}
		if gb := gapBounds(b, layer, n, i); g.Defined() && w.wantGap(d, gb) {
			contains := func(c *Cursor) bool {
				if !hasPrefix(c.Path, d.path) {
					return false
				}
				if len(c.Path) > len(d.path) {
					return gb.has(c.Path[len(d.path)])
				}
				return c.Link != LinkDir && gb.has(c.Key)
			}
			err := w.item(l, g, kindNode, func() bool { return w.subtreeShared(d, g) }, contains,
				func(data []byte) error { return w.node(d, data, gb) })
			if err != nil {
				return err
			}
		}
		if i < len(n.Entries) && w.wantEntry(d, n.Entries[i].Key) {
			if err := w.entry(d, n.Entries[i].Key, n.Entries[i].Val); err != nil {
				return err
			}
		}
	}
	return nil
}

// entry walks one directory entry: its target (subdirectory or inline blob), then its meta.
func (w *treeWalk) entry(d *dirCtx, k string, e tree.Entry) error {
	sub, full := modeFull, true // subdirectory mode; whether blob and meta are included
	var rng *NameRange
	switch {
	case d.mode == modePath && len(d.path)+1 < len(w.req.Path):
		sub, full = modePath, false
	case d.mode == modePath: // the requested entry
		sub, rng = w.targetMode(), w.req.NameRange
		full = w.req.Scope != ScopeNode
	case d.mode == modeList:
		sub = modeSkip
	}
	var as []tree.Entry // have entries at k (server, lazily)
	haveAt := func() []tree.Entry {
		if as == nil {
			as = w.haveEntries(d, k)
		}
		return as
	}
	switch {
	case e.Kind == tree.KindDir && sub != modeSkip:
		cd := &dirCtx{path: append(slices.Clip(d.path), k), mode: sub, rng: rng}
		if w.hst != nil && len(d.have) > 0 {
			for _, a := range haveAt() {
				if a.Kind == tree.KindDir {
					cd.have = append(cd.have, haveDir{root: a.CID, t: tree.Dir(w.hst, a.CID)})
				}
			}
		}
		shared := func() bool {
			for _, a := range cd.have {
				if a.root == e.CID {
					return true
				}
			}
			return false
		}
		if err := w.dir(cd, e.CID, shared); err != nil {
			return err
		}
	case e.Kind != tree.KindDir && full && e.Size <= w.req.inline():
		shared := func() bool {
			for _, a := range haveAt() {
				if a.CID == e.CID {
					return true
				}
			}
			return false
		}
		if err := w.item(&loc{path: d.path, key: k, link: LinkTarget}, e.CID, kindBlob, shared, nil, nil); err != nil {
			return err
		}
	}
	if full && e.Meta.Defined() {
		shared := func() bool {
			for _, a := range haveAt() {
				if a.Meta == e.Meta {
					return true
				}
			}
			return false
		}
		return w.item(&loc{path: d.path, key: k, link: LinkMeta}, e.Meta, kindNode, shared, nil, func([]byte) error { return nil })
	}
	return nil
}

// haveRoots resolves the request's have roots (headers or directory roots) that the server
// holds; it is empty on the client.
func (w *treeWalk) haveRoots() []haveDir {
	if w.hst == nil {
		return nil
	}
	var out []haveDir
	for _, c := range w.req.Have {
		data, err := w.hst.Get(c)
		if err != nil || c.Verify(data) != nil {
			continue
		}
		switch kind, _ := classifyRoot(data); kind {
		case rootHeader:
			if h, err := tree.DecodeHeader(data); err == nil {
				out = append(out, haveDir{root: h.Root, t: tree.Dir(w.hst, h.Root)})
			}
		case rootDir:
			out = append(out, haveDir{root: c, t: tree.Dir(w.hst, c)})
		}
	}
	return out
}

// haveEntries returns the entries named k in the have directories. Have trees the server
// cannot read count as absent: it then sends more, never less.
func (w *treeWalk) haveEntries(d *dirCtx, k string) []tree.Entry {
	out := []tree.Entry{}
	for _, a := range d.have {
		if e, ok, err := a.t.Get(k); err == nil && ok {
			out = append(out, e)
		}
	}
	return out
}

// subtreeShared reports whether MST subtree g of directory d is a subtree of a have
// directory: it then lies on that directory's search path for g's first name.
func (w *treeWalk) subtreeShared(d *dirCtx, g cid.CID) bool {
	if len(d.have) == 0 {
		return false
	}
	n, err := w.dirNode(g)
	if err != nil || len(n.Entries) == 0 {
		return false
	}
	k := n.Entries[0].Key
	for _, a := range d.have {
		for c, depth := a.root, 0; c.Defined() && depth < 256; depth++ {
			if c == g {
				return true
			}
			an, err := w.dirNode(c)
			if err != nil {
				break
			}
			i := sort.Search(len(an.Entries), func(i int) bool { return an.Entries[i].Key >= k })
			if i < len(an.Entries) && an.Entries[i].Key == k {
				break
			}
			if c = an.Left; i > 0 {
				c = an.Entries[i-1].Right
			}
		}
	}
	return false
}

func (w *treeWalk) dirNode(c cid.CID) (*mst.Node[tree.Entry], error) {
	if n, ok := w.cache[c]; ok {
		return n, nil
	}
	data, err := w.hst.Get(c)
	if err != nil {
		return nil, err
	}
	n, err := tree.DirProfile.Codec.Decode(data)
	if err != nil {
		return nil, err
	}
	if len(w.cache) < 1<<16 {
		w.cache[c] = n
	}
	return n, nil
}
