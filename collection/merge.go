package collection

import (
	"slices"
	"strings"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
)

// Union heads (D22): the effective catalog of a `merge: union` collection is the union, by
// path, of the latest catalog of every writer, with admin drops applied afterwards.
//
// The merge decides each leaf path p (an item, or a plain file outside items)
// independently: walking the writers in rank order, the first writer whose tree has
// anything at p decides. If that is a leaf at p, it is the merged entry; if it is a
// directory at p (items below p) or a leaf at a prefix of p, nothing is merged at p.
// Later writers with a different leaf at p are conflict losers ("entry"), and their leaves
// at a path the decider blocks are structural losers ("structure"). Identical entries are
// one item. A path at or below a dropped path is never merged. Because each path's
// outcome depends only on the writer trees, the merged tree is a pure function of
// (ranked writer roots, drops), and canonical MSTs make its root deterministic however it
// was computed. Merged directories carry no metadata and their size is the sum of their
// entries.

// Writer is one writer's catalog: the DID and its head's root directory (zero: none).
type Writer struct {
	DID  string
	Root cid.CID
}

// Leaf is a merged entry and the writer that supplies it.
type Leaf struct {
	Entry  tree.Entry
	Writer string
}

// Conflict reasons.
const (
	ReasonEntry     = "entry"
	ReasonStructure = "structure"
)

// Loser is a writer's leaf that lost to an earlier-ranked writer at the same path.
type Loser struct {
	Writer string
	Entry  tree.Entry
	Reason string
}

// Union holds a merged catalog and incrementally maintains it as writer heads change.
type Union struct {
	St      mst.Store
	Writers []Writer // rank order
	Drop    []string // dropped paths (normalised, no leading/trailing slash)
	Root    cid.CID  // merged root directory; zero is empty
	Size    uint64   // its size

	// OnItem reports a change of the merged view at path: old is the previous entry (nil
	// if none), new the new leaf (nil if removed). It may be called with old equal to
	// new.Entry when only the supplying writer changed.
	OnItem func(path string, old *tree.Entry, new *Leaf) error
	// OnDecide reports the outcome at every re-decided path, so conflict records can be
	// replaced (losers is empty when the path has no conflict).
	OnDecide func(path string, win *Leaf, losers []Loser) error
}

type status int

const (
	absent status = iota
	leaf
	container
	blocked
)

func split(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// statusAt reports what a tree has at path segs.
func statusAt(st mst.Store, root cid.CID, segs []string) (status, tree.Entry, error) {
	if !root.Defined() {
		return absent, tree.Entry{}, nil
	}
	cur := root
	for i, s := range segs {
		e, ok, err := tree.Dir(st, cur).Get(s)
		if err != nil {
			return absent, tree.Entry{}, err
		}
		if !ok {
			return absent, tree.Entry{}, nil
		}
		last := i == len(segs)-1
		switch {
		case last && tree.IsContainer(e):
			return container, e, nil
		case last:
			return leaf, e, nil
		case !tree.IsContainer(e):
			return blocked, e, nil
		}
		cur = e.CID
	}
	return container, tree.Entry{}, nil
}

// walkLeaves calls fn for every leaf at or below dir (a container directory root).
func walkLeaves(st mst.Store, dir cid.CID, prefix string, fn func(string, tree.Entry) error) error {
	if !dir.Defined() {
		return nil
	}
	return tree.Dir(st, dir).Walk("", func(k string, e tree.Entry) (bool, error) {
		p := join(prefix, k)
		if tree.IsContainer(e) {
			return true, walkLeaves(st, e.CID, p, fn)
		}
		return true, fn(p, e)
	})
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}

// Leaves calls fn for every leaf of a catalog rooted at directory root.
func Leaves(st mst.Store, root cid.CID, fn func(path string, e tree.Entry) error) error {
	return walkLeaves(st, root, "", fn)
}

// diffLeaves reports every leaf path that differs between container directories a and b.
func diffLeaves(st mst.Store, prefix string, a, b cid.CID, fn func(string)) error {
	return mst.Diff(tree.Dir(st, a), tree.Dir(st, b), func(c mst.Change[tree.Entry]) error {
		p := join(prefix, c.Key)
		oc := c.Old != nil && tree.IsContainer(*c.Old)
		nc := c.New != nil && tree.IsContainer(*c.New)
		if oc && nc {
			return diffLeaves(st, p, c.Old.CID, c.New.CID, fn)
		}
		emit := func(e *tree.Entry, isC bool) error {
			switch {
			case e == nil:
				return nil
			case isC:
				return walkLeaves(st, e.CID, p, func(q string, _ tree.Entry) error { fn(q); return nil })
			}
			fn(p)
			return nil
		}
		if err := emit(c.Old, oc); err != nil {
			return err
		}
		return emit(c.New, nc)
	})
}

func (u *Union) dropped(p string) bool {
	for _, d := range u.Drop {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// decide computes the merged outcome at path p.
func (u *Union) decide(p string) (*Leaf, []Loser, error) {
	if u.dropped(p) {
		return nil, nil, nil
	}
	segs := split(p)
	var win *Leaf
	var losers []Loser
	decided := false
	for _, w := range u.Writers {
		st, e, err := statusAt(u.St, w.Root, segs)
		if err != nil {
			return nil, nil, err
		}
		switch st {
		case absent:
			continue
		case leaf:
			switch {
			case !decided:
				win, decided = &Leaf{Entry: e, Writer: w.DID}, true
			case win == nil:
				losers = append(losers, Loser{Writer: w.DID, Entry: e, Reason: ReasonStructure})
			case e != win.Entry:
				losers = append(losers, Loser{Writer: w.DID, Entry: e, Reason: ReasonEntry})
			}
		default:
			decided = true
		}
	}
	return win, losers, nil
}

// Update moves the merged catalog to new writer roots and drops, re-deciding only the
// paths whose outcome can change: leaves that differ in some writer, their prefixes and
// the leaves below them, and everything under changed drops. A change of the writers'
// relative rank re-decides every leaf.
func (u *Union) Update(writers []Writer, drop []string) error {
	ed, err := newEditor(u.St, u.Root, u.Size)
	if err != nil {
		return err
	}
	queue := map[string]bool{}
	var changed []string
	oldRoots := map[string]cid.CID{}
	for _, w := range u.Writers {
		oldRoots[w.DID] = w.Root
	}
	newRoots := map[string]cid.CID{}
	for _, w := range writers {
		newRoots[w.DID] = w.Root
	}
	if !sameRelativeOrder(u.Writers, writers) {
		for _, ws := range [][]Writer{u.Writers, writers} {
			for _, w := range ws {
				if err := Leaves(u.St, w.Root, func(p string, _ tree.Entry) error { queue[p] = true; return nil }); err != nil {
					return err
				}
			}
		}
		if err := ed.leavesUnder(nil, func(p string, _ tree.Entry) error { queue[p] = true; return nil }); err != nil {
			return err
		}
	} else {
		dids := map[string]bool{}
		for d := range oldRoots {
			dids[d] = true
		}
		for d := range newRoots {
			dids[d] = true
		}
		for d := range dids {
			if oldRoots[d] == newRoots[d] {
				continue
			}
			if err := diffLeaves(u.St, "", oldRoots[d], newRoots[d], func(p string) { changed = append(changed, p) }); err != nil {
				return err
			}
		}
	}
	changed = append(changed, symDiff(u.Drop, drop)...)
	u.Writers, u.Drop = slices.Clone(writers), slices.Clone(drop)
	// The paths whose outcome may change: each changed path, its prefixes, and the leaves
	// below it in any writer or in the merged tree.
	for _, p := range changed {
		segs := split(p)
		for i := 1; i <= len(segs); i++ {
			queue[strings.Join(segs[:i], "/")] = true
		}
		for _, w := range u.Writers {
			st, e, err := statusAt(u.St, w.Root, segs)
			if err != nil {
				return err
			}
			if st == container {
				if err := walkLeaves(u.St, e.CID, p, func(q string, _ tree.Entry) error { queue[q] = true; return nil }); err != nil {
					return err
				}
			}
		}
		if err := ed.leavesUnder(segs, func(q string, _ tree.Entry) error { queue[q] = true; return nil }); err != nil {
			return err
		}
	}
	paths := make([]string, 0, len(queue))
	for p := range queue {
		paths = append(paths, p)
	}
	slices.Sort(paths) // prefixes before the paths below them
	ed.onRemoved = func(p string, e tree.Entry) error {
		if u.OnItem != nil {
			return u.OnItem(p, &e, nil)
		}
		return nil
	}
	for _, p := range paths {
		if err := u.process(ed, p); err != nil {
			return err
		}
	}
	root, err := ed.flush()
	if err != nil {
		return err
	}
	u.Root, u.Size = root, ed.root.size
	return nil
}

func (u *Union) process(ed *editor, p string) error {
	win, losers, err := u.decide(p)
	if err != nil {
		return err
	}
	if u.OnDecide != nil {
		if err := u.OnDecide(p, win, losers); err != nil {
			return err
		}
	}
	segs := split(p)
	st, cur, err := ed.lookup(segs)
	if err != nil {
		return err
	}
	switch {
	case win != nil:
		if st == leaf && cur == win.Entry {
			if u.OnItem != nil {
				return u.OnItem(p, &cur, win)
			}
			return nil
		}
		if err := ed.put(segs, win.Entry); err != nil {
			return err
		}
		if u.OnItem != nil {
			var old *tree.Entry
			if st == leaf {
				old = &cur
			}
			return u.OnItem(p, old, win)
		}
	case st == leaf:
		if err := ed.del(segs); err != nil {
			return err
		}
		if u.OnItem != nil {
			return u.OnItem(p, &cur, nil)
		}
	}
	return nil
}

// sameRelativeOrder reports whether the writers common to a and b appear in the same order.
func sameRelativeOrder(a, b []Writer) bool {
	inB := map[string]bool{}
	for _, w := range b {
		inB[w.DID] = true
	}
	inA := map[string]bool{}
	for _, w := range a {
		inA[w.DID] = true
	}
	var x, y []string
	for _, w := range a {
		if inB[w.DID] {
			x = append(x, w.DID)
		}
	}
	for _, w := range b {
		if inA[w.DID] {
			y = append(y, w.DID)
		}
	}
	return slices.Equal(x, y)
}

func symDiff(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	for _, x := range b {
		if !slices.Contains(a, x) {
			out = append(out, x)
		}
	}
	return out
}

// Merge computes the merged catalog of writers from scratch (into st); it equals what
// any sequence of Updates reaching the same writers and drops produces.
func Merge(st mst.Store, writers []Writer, drop []string) (cid.CID, uint64, error) {
	u := &Union{St: st}
	if err := u.Update(writers, drop); err != nil {
		return cid.CID{}, 0, err
	}
	return u.Root, u.Size, nil
}

// editor edits a nested directory tree in place, keeping the directories it opened in
// memory until flush.
type editor struct {
	st        mst.Store
	root      *dnode
	empty     cid.CID
	onRemoved func(string, tree.Entry) error
}

type dnode struct {
	t     *mst.Tree[tree.Entry]
	size  uint64
	kids  map[string]*dnode
	dirty bool
}

func newEditor(st mst.Store, root cid.CID, size uint64) (*editor, error) {
	empty, err := tree.Dir(mst.MemStore{}, cid.CID{}).Flush()
	if err != nil {
		return nil, err
	}
	return &editor{st: st, empty: empty, root: &dnode{t: tree.Dir(st, root), size: size, kids: map[string]*dnode{}}}, nil
}

// lookup reports what the merged tree has at segs.
func (ed *editor) lookup(segs []string) (status, tree.Entry, error) {
	n := ed.root
	for i, s := range segs {
		last := i == len(segs)-1
		if k := n.kids[s]; k != nil {
			if last {
				return container, tree.Entry{}, nil
			}
			n = k
			continue
		}
		e, ok, err := n.t.Get(s)
		if err != nil {
			return absent, tree.Entry{}, err
		}
		switch {
		case !ok:
			return absent, tree.Entry{}, nil
		case last && tree.IsContainer(e):
			return container, e, nil
		case last:
			return leaf, e, nil
		case !tree.IsContainer(e):
			return blocked, e, nil
		}
		n = ed.open(n, s, e)
	}
	return container, tree.Entry{}, nil
}

func (ed *editor) open(n *dnode, name string, e tree.Entry) *dnode {
	k := &dnode{t: tree.Dir(ed.st, e.CID), size: e.Size, kids: map[string]*dnode{}}
	n.kids[name] = k
	return k
}

// leavesUnder calls fn for every merged leaf at or below segs.
func (ed *editor) leavesUnder(segs []string, fn func(string, tree.Entry) error) error {
	n := ed.root
	for i, s := range segs {
		if k := n.kids[s]; k != nil {
			n = k
			continue
		}
		e, ok, err := n.t.Get(s)
		if err != nil || !ok {
			return err
		}
		if !tree.IsContainer(e) {
			if i == len(segs)-1 {
				return fn(strings.Join(segs, "/"), e)
			}
			return nil
		}
		n = ed.open(n, s, e)
	}
	return ed.walk(n, strings.Join(segs, "/"), fn)
}

func (ed *editor) walk(n *dnode, prefix string, fn func(string, tree.Entry) error) error {
	return n.t.Walk("", func(k string, e tree.Entry) (bool, error) {
		p := join(prefix, k)
		if kid := n.kids[k]; kid != nil {
			return true, ed.walk(kid, p, fn)
		}
		if tree.IsContainer(e) {
			return true, walkLeaves(ed.st, e.CID, p, fn)
		}
		return true, fn(p, e)
	})
}

func adjust(path []*dnode, delta int64) {
	for _, d := range path {
		d.size = uint64(int64(d.size) + delta)
		d.dirty = true
	}
}

// put sets the leaf at segs, replacing a leaf at a prefix or a directory at segs (whose
// leaves are reported removed).
func (ed *editor) put(segs []string, e tree.Entry) error {
	n := ed.root
	path := []*dnode{n}
	for i, s := range segs[:len(segs)-1] {
		kid := n.kids[s]
		if kid == nil {
			old, ok, err := n.t.Get(s)
			if err != nil {
				return err
			}
			if ok && tree.IsContainer(old) {
				kid = ed.open(n, s, old)
			} else {
				if ok {
					if err := ed.removed(strings.Join(segs[:i+1], "/"), old); err != nil {
						return err
					}
					adjust(path, -int64(old.Size))
				}
				if err := n.t.Put(s, tree.Entry{Kind: tree.KindDir, CID: ed.empty}); err != nil {
					return err
				}
				kid = &dnode{t: tree.Dir(ed.st, cid.CID{}), kids: map[string]*dnode{}}
				n.kids[s] = kid
			}
		}
		n = kid
		path = append(path, n)
	}
	name := segs[len(segs)-1]
	p := strings.Join(segs, "/")
	var oldSize uint64
	if kid := n.kids[name]; kid != nil {
		if err := ed.walk(kid, p, ed.removed); err != nil {
			return err
		}
		oldSize = kid.size
		delete(n.kids, name)
	} else if old, ok, err := n.t.Get(name); err != nil {
		return err
	} else if ok {
		if tree.IsContainer(old) {
			if err := walkLeaves(ed.st, old.CID, p, ed.removed); err != nil {
				return err
			}
		}
		oldSize = old.Size
	}
	if err := n.t.Put(name, e); err != nil {
		return err
	}
	adjust(path, int64(e.Size)-int64(oldSize))
	return nil
}

func (ed *editor) removed(p string, e tree.Entry) error {
	if ed.onRemoved != nil {
		return ed.onRemoved(p, e)
	}
	return nil
}

// del removes the leaf at segs (if there is one).
func (ed *editor) del(segs []string) error {
	n := ed.root
	path := []*dnode{n}
	for _, s := range segs[:len(segs)-1] {
		kid := n.kids[s]
		if kid == nil {
			old, ok, err := n.t.Get(s)
			if err != nil || !ok || !tree.IsContainer(old) {
				return err
			}
			kid = ed.open(n, s, old)
		}
		n = kid
		path = append(path, n)
	}
	name := segs[len(segs)-1]
	if n.kids[name] != nil {
		return nil
	}
	old, ok, err := n.t.Get(name)
	if err != nil || !ok || tree.IsContainer(old) {
		return err
	}
	if _, err := n.t.Delete(name); err != nil {
		return err
	}
	adjust(path, -int64(old.Size))
	return nil
}

// flush writes the dirty directories bottom-up, dropping emptied ones, and returns the
// root.
func (ed *editor) flush() (cid.CID, error) {
	c, _, err := ed.flushNode(ed.root)
	return c, err
}

func (ed *editor) flushNode(n *dnode) (cid.CID, bool, error) {
	names := make([]string, 0, len(n.kids))
	for k := range n.kids {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, name := range names {
		kid := n.kids[name]
		if !kid.dirty {
			continue
		}
		c, empty, err := ed.flushNode(kid)
		if err != nil {
			return cid.CID{}, false, err
		}
		if empty {
			if _, err := n.t.Delete(name); err != nil {
				return cid.CID{}, false, err
			}
			delete(n.kids, name)
		} else if err := n.t.Put(name, tree.Entry{Kind: tree.KindDir, CID: c, Size: kid.size}); err != nil {
			return cid.CID{}, false, err
		}
		kid.dirty = false
	}
	c, err := n.t.Flush()
	if err != nil {
		return cid.CID{}, false, err
	}
	empty := true
	err = n.t.Walk("", func(string, tree.Entry) (bool, error) { empty = false; return false, nil })
	return c, empty, err
}
