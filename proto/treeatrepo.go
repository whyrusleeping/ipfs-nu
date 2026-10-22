package proto

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
)

// ProfileAtrepo walks a v3 commit, its layered MST and record leaves. Blob policy
// is deliberately separate: atrepo+blobs uses this wire profile, then layer 1.
const ProfileAtrepo = "atrepo"

func (r *TreeRequest) checkAtrepo() error {
	bad := func() error { return fmt.Errorf("%w: invalid atrepo path or cursor", errRequest) }
	if r.Root.Hash() != cid.SHA256 || len(r.Path) > 1 || (len(r.Path) == 1 && atrepo.ValidMSTKey(r.Path[0]) != nil) {
		return bad()
	}
	if c := r.Cursor; c != nil {
		if len(c.Path) > atrepo.MaxLayer || (c.Link != LinkRoot && c.Link != LinkDir && c.Link != LinkTarget) {
			return bad()
		}
		if c.Link == LinkRoot && len(c.Path) != 0 {
			return bad()
		}
		if (c.Link == LinkTarget) != (c.Key != "") || (c.Key != "" && atrepo.ValidMSTKey(c.Key) != nil) {
			return bad()
		}
		for _, edge := range c.Path {
			if edge != "l" && (!strings.HasPrefix(edge, "t:") || atrepo.ValidMSTKey(strings.TrimPrefix(edge, "t:")) != nil) {
				return bad()
			}
		}
	}
	return nil
}

type atWalk struct {
	*treeWalk
	have  []haveRepo
	cache map[cid.CID]*mst.Node[cid.CID]
}
type haveRepo struct {
	root cid.CID
	tree *mst.Tree[cid.CID]
}

func (w *treeWalk) atrepo() error {
	a := &atWalk{treeWalk: w, cache: map[cid.CID]*mst.Node[cid.CID]{}}
	err := w.item(&loc{link: LinkRoot}, w.req.Root, kindNode, func() bool { return slices.Contains(w.req.Have, w.req.Root) }, func(*Cursor) bool { return true }, func(data []byte) error {
		com, err := atrepo.DecodeCommit(data)
		if err != nil {
			return fmt.Errorf("%w: %w", errInvalidTree, err)
		}
		if w.hst != nil {
			for _, c := range w.req.Have {
				b, err := w.hst.Get(c)
				if err != nil || c.Verify(b) != nil {
					continue
				}
				h, err := atrepo.DecodeCommit(b)
				if err == nil && h.DID == com.DID {
					a.have = append(a.have, haveRepo{h.Data, atrepo.LoadTree(w.hst, h.Data)})
				}
			}
		}
		return a.sub(nil, com.Data, topBounds)
	})
	if err == nil && w.seek != nil {
		return errCursor
	}
	return err
}

func (a *atWalk) sub(path []string, c cid.CID, b kbounds) error {
	return a.item(&loc{path: path, link: LinkDir}, c, kindNode, func() bool { return a.shared(c) }, func(cur *Cursor) bool { return hasPrefix(cur.Path, path) }, func(data []byte) error {
		n, layer, err := decodeAtNode(data, b)
		if err != nil {
			return err
		}
		if len(a.req.Path) == 0 && a.req.Scope == ScopeNode {
			return nil
		}
		for i := 0; i <= len(n.Entries); i++ {
			child, edge := n.Left, "l"
			g := kbounds{lo: b.lo, hi: b.hi, hasLo: b.hasLo, hasHi: b.hasHi, below: layer}
			if i > 0 {
				e := n.Entries[i-1]
				child, edge = e.Right, "t:"+e.Key
				g.lo, g.hasLo = e.Key, true
			}
			if i < len(n.Entries) {
				g.hi, g.hasHi = n.Entries[i].Key, true
			}
			want := true
			if len(a.req.Path) > 0 {
				want = g.has(a.req.Path[0])
			} else if r := a.req.NameRange; r != nil {
				want = !(g.hasHi && g.hi <= r.Start) && !(g.hasLo && r.End != "" && g.lo >= r.End)
			}
			if child.Defined() && want {
				if err := a.sub(append(slices.Clip(path), edge), child, g); err != nil {
					return err
				}
			}
			if i == len(n.Entries) {
				continue
			}
			e := n.Entries[i]
			if (len(a.req.Path) > 0 && e.Key != a.req.Path[0]) || !a.req.NameRange.has(e.Key) {
				continue
			}
			shared := func() bool {
				for _, h := range a.have {
					v, ok, err := h.tree.Get(e.Key)
					if err == nil && ok && v == e.Val {
						return true
					}
				}
				return false
			}
			if err := a.item(&loc{path: path, key: e.Key, link: LinkTarget}, e.Val, kindNode, shared, nil, func(data []byte) error {
				if len(data) > atrepo.MaxRecordSize {
					return fmt.Errorf("%w: record too large", errInvalidTree)
				}
				_, err := drisl.AtprotoRecord.Decode(data)
				return err
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// Empty intermediate nodes consume exactly one layer, retaining the same key
// interval. The root cannot be an intermediate; only an empty repo has an empty leaf.
func decodeAtNode(data []byte, b kbounds) (*mst.Node[cid.CID], int, error) {
	n, err := atrepo.MST.Codec.Decode(data)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", errInvalidTree, err)
	}
	bad := func() (*mst.Node[cid.CID], int, error) {
		return nil, 0, fmt.Errorf("%w: atrepo MST bounds/layer", errInvalidTree)
	}
	if len(n.Entries) == 0 {
		if b == topBounds && !n.Left.Defined() {
			return n, 0, nil
		}
		if b == topBounds || b.below <= 1 || !n.Left.Defined() {
			return bad()
		}
		return n, b.below - 1, nil
	}
	layer := atrepo.MST.Layer(n.Entries[0].Key)
	if len(n.Entries) > atrepo.MaxNodeEntries || layer > atrepo.MaxLayer || (b != topBounds && layer != b.below-1) {
		return bad()
	}
	for i, e := range n.Entries {
		if atrepo.ValidMSTKey(e.Key) != nil || !b.has(e.Key) || (i > 0 && e.Key <= n.Entries[i-1].Key) || atrepo.MST.Layer(e.Key) != layer || e.Val.Codec() != cid.DRISL || e.Val.Hash() != cid.SHA256 {
			return bad()
		}
	}
	return n, layer, nil
}

func (a *atWalk) node(c cid.CID) (*mst.Node[cid.CID], error) {
	if n := a.cache[c]; n != nil {
		return n, nil
	}
	b, err := a.hst.Get(c)
	if err != nil {
		return nil, err
	}
	if err = c.Verify(b); err != nil {
		return nil, err
	}
	n, err := atrepo.MST.Codec.Decode(b)
	if err == nil && len(a.cache) < 1<<16 {
		a.cache[c] = n
	}
	return n, err
}

// A shared subtree is on the held tree's search path for any of its keys,
// including all empty intermediate nodes on that path. No whole-repo index.
func (a *atWalk) shared(c cid.CID) bool {
	if len(a.have) == 0 {
		return false
	}
	key := ""
	for cur, depth := c, 0; cur.Defined() && depth <= atrepo.MaxLayer; depth++ {
		n, err := a.node(cur)
		if err != nil {
			return false
		}
		if len(n.Entries) > 0 {
			key = n.Entries[0].Key
			break
		}
		cur = n.Left
	}
	for _, h := range a.have {
		for cur, depth := h.root, 0; cur.Defined() && depth <= atrepo.MaxLayer; depth++ {
			if cur == c {
				return true
			}
			n, err := a.node(cur)
			if err != nil {
				break
			}
			i := sort.Search(len(n.Entries), func(i int) bool { return n.Entries[i].Key >= key })
			if i < len(n.Entries) && n.Entries[i].Key == key {
				break
			}
			cur = n.Left
			if i > 0 {
				cur = n.Entries[i-1].Right
			}
		}
	}
	return false
}
