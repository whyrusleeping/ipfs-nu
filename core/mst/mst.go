// Package mst implements one Merkle Search Tree with profiles (D3, D21). A profile fixes the
// depth hash granularity (SHA-256 of the key, counted in LevelBits-bit units), the node
// schema (Codec), value form and limits. The Nu directory profile lives in core/tree; the
// atproto v3 profile (2-bit units, prefix-compressed {l, e: [{p, k, v, t}]}) is a second
// Codec/LevelBits pair.
//
// Structure: the tree for a key set S is a node holding, in order, the keys of S with the
// highest layer, and between (and around) them the trees of the remaining keys in each gap.
// Empty gaps have no subtree and no empty nodes exist except the root of an empty tree, so
// the root CID depends only on the key/value set.
package mst

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strings"

	"github.com/whyrusleeping/nu/core/cid"
)

// Store holds verified-by-CID structure blocks. Get returns an error wrapping ErrNotFound
// for missing blocks.
type Store interface {
	Get(c cid.CID) ([]byte, error)
	Put(c cid.CID, data []byte) error
}

var (
	ErrNotFound     = errors.New("mst: block not found")
	ErrInvalidNode  = errors.New("mst: invalid node")
	ErrNodeTooLarge = errors.New("mst: node exceeds the node size limit")
)

// Node is the schema-neutral form of one MST node: an optional left subtree and entries,
// each with an optional subtree to its right. Zero CIDs mean no subtree.
type Node[V any] struct {
	Left    cid.CID
	Entries []Entry[V]
}

// Entry is one key in a node.
type Entry[V any] struct {
	Key   string
	Val   V
	Right cid.CID
}

// Codec encodes and decodes the node schema of a profile. Decode must reject any encoding
// that Encode would not produce.
type Codec[V any] interface {
	Encode(n *Node[V]) ([]byte, error)
	Decode(data []byte) (*Node[V], error)
}

// Profile parameterises the tree.
type Profile[V any] struct {
	Name        string
	LevelBits   int // layer = leading zero bits of SHA-256(key) / LevelBits
	MaxNodeSize int // encoded node limit, applied when producing and when loading
	Codec       Codec[V]
	ValidKey    func(key string) error // nil accepts any key
	Equal       func(a, b V) bool      // value equality for Diff
}

// Layer returns the layer of key.
func (p *Profile[V]) Layer(key string) int {
	h := sha256.Sum256([]byte(key))
	z := 0
	for _, b := range h {
		if b != 0 {
			z += bits.LeadingZeros8(b)
			break
		}
		z += 8
	}
	return z / p.LevelBits
}

type ref[V any] struct {
	c cid.CID  // zero while dirty
	n *node[V] // nil until loaded
}

type node[V any] struct {
	layer int
	left  *ref[V]
	ents  []ent[V]
}

type ent[V any] struct {
	key   string
	val   V
	right *ref[V]
}

// gap returns the subtree in gap i (0 = left of the first entry).
func (n *node[V]) gap(i int) *ref[V] {
	if i == 0 {
		return n.left
	}
	return n.ents[i-1].right
}

func (n *node[V]) clone() *node[V] {
	return &node[V]{layer: n.layer, left: n.left, ents: append([]ent[V](nil), n.ents...)}
}

// mut returns a node to modify in place of r's: r's own node while it is dirty (unflushed
// nodes belong only to this tree version), otherwise a copy under a new ref.
func mut[V any](r *ref[V]) (*node[V], *ref[V]) {
	if !r.c.Defined() {
		return r.n, r
	}
	c := r.n.clone()
	return c, &ref[V]{n: c}
}

func (n *node[V]) setGap(i int, r *ref[V]) {
	if i == 0 {
		n.left = r
	} else {
		n.ents[i-1].right = r
	}
}

func (n *node[V]) search(key string) int {
	return sort.Search(len(n.ents), func(i int) bool { return n.ents[i].key >= key })
}

// bounds constrains a subtree loaded from an untrusted store: keys strictly inside (lo, hi)
// and layer below the parent's.
type bounds struct {
	lo, hi       string
	hasLo, hasHi bool
	below        int
}

var top = bounds{below: math.MaxInt}

func (n *node[V]) gapBounds(b bounds, i int) bounds {
	g := bounds{lo: b.lo, hi: b.hi, hasLo: b.hasLo, hasHi: b.hasHi, below: n.layer}
	if i > 0 {
		g.lo, g.hasLo = n.ents[i-1].key, true
	}
	if i < len(n.ents) {
		g.hi, g.hasHi = n.ents[i].key, true
	}
	return g
}

// Tree is a handle on one tree version. Mutations build new nodes in memory; Flush writes
// them. A Tree is not safe for concurrent use.
type Tree[V any] struct {
	p    *Profile[V]
	st   Store
	root *ref[V]
}

// Load opens the tree with root c (the zero CID opens an empty tree).
func (p *Profile[V]) Load(st Store, c cid.CID) *Tree[V] {
	t := &Tree[V]{p: p, st: st}
	if c.Defined() {
		t.root = &ref[V]{c: c}
	}
	return t
}

func (t *Tree[V]) load(r *ref[V], b bounds) (*node[V], error) {
	if r.n != nil {
		return r.n, nil
	}
	data, err := t.st.Get(r.c)
	if err != nil {
		return nil, err
	}
	if len(data) > t.p.MaxNodeSize {
		return nil, ErrNodeTooLarge
	}
	if err := r.c.Verify(data); err != nil {
		return nil, err
	}
	if r.c.Codec() != cid.DRISL {
		return nil, fmt.Errorf("%w: not a DRISL block", ErrInvalidNode)
	}
	w, err := t.p.Codec.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidNode, err)
	}
	n := &node[V]{}
	if w.Left.Defined() {
		n.left = &ref[V]{c: w.Left}
	}
	if len(w.Entries) == 0 {
		if b != top || n.left != nil {
			return nil, fmt.Errorf("%w: empty node", ErrInvalidNode)
		}
		r.n = n
		return n, nil
	}
	n.layer = t.p.Layer(w.Entries[0].Key)
	if n.layer >= b.below {
		return nil, fmt.Errorf("%w: layer %d not below parent layer %d", ErrInvalidNode, n.layer, b.below)
	}
	n.ents = make([]ent[V], len(w.Entries))
	for i, e := range w.Entries {
		if t.p.ValidKey != nil {
			if err := t.p.ValidKey(e.Key); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidNode, err)
			}
		}
		if i > 0 && e.Key <= w.Entries[i-1].Key {
			return nil, fmt.Errorf("%w: keys out of order", ErrInvalidNode)
		}
		if t.p.Layer(e.Key) != n.layer {
			return nil, fmt.Errorf("%w: mixed layers", ErrInvalidNode)
		}
		n.ents[i] = ent[V]{key: e.Key, val: e.Val}
		if e.Right.Defined() {
			n.ents[i].right = &ref[V]{c: e.Right}
		}
	}
	if (b.hasLo && n.ents[0].key <= b.lo) || (b.hasHi && n.ents[len(n.ents)-1].key >= b.hi) {
		return nil, fmt.Errorf("%w: key outside its subtree range", ErrInvalidNode)
	}
	r.n = n
	return n, nil
}

// Get looks key up, loading only the nodes on its search path.
func (t *Tree[V]) Get(key string) (V, bool, error) {
	var zero V
	r, b := t.root, top
	for r != nil {
		n, err := t.load(r, b)
		if err != nil {
			return zero, false, err
		}
		i := n.search(key)
		if i < len(n.ents) && n.ents[i].key == key {
			return n.ents[i].val, true, nil
		}
		r, b = n.gap(i), n.gapBounds(b, i)
	}
	return zero, false, nil
}

// Put inserts or replaces key.
func (t *Tree[V]) Put(key string, val V) error {
	if t.p.ValidKey != nil {
		if err := t.p.ValidKey(key); err != nil {
			return err
		}
	}
	r, err := t.insert(t.root, top, key, val, t.p.Layer(key))
	if err != nil {
		return err
	}
	t.root = r
	return nil
}

func (t *Tree[V]) insert(r *ref[V], b bounds, key string, val V, layer int) (*ref[V], error) {
	if r == nil {
		return &ref[V]{n: &node[V]{layer: layer, ents: []ent[V]{{key: key, val: val}}}}, nil
	}
	n, err := t.load(r, b)
	if err != nil {
		return nil, err
	}
	if len(n.ents) == 0 { // empty root
		return t.insert(nil, b, key, val, layer)
	}
	if layer > n.layer {
		l, rr, err := t.split(r, b, key)
		if err != nil {
			return nil, err
		}
		return &ref[V]{n: &node[V]{layer: layer, left: l, ents: []ent[V]{{key: key, val: val, right: rr}}}}, nil
	}
	i := n.search(key)
	if i < len(n.ents) && n.ents[i].key == key {
		c, nr := mut(r)
		c.ents[i].val = val
		return nr, nil
	}
	if layer == n.layer {
		l, rr, err := t.split(n.gap(i), n.gapBounds(b, i), key)
		if err != nil {
			return nil, err
		}
		c, nr := mut(r)
		c.setGap(i, l)
		c.ents = append(c.ents, ent[V]{})
		copy(c.ents[i+1:], c.ents[i:])
		c.ents[i] = ent[V]{key: key, val: val, right: rr}
		return nr, nil
	}
	sub, err := t.insert(n.gap(i), n.gapBounds(b, i), key, val, layer)
	if err != nil {
		return nil, err
	}
	c, nr := mut(r)
	c.setGap(i, sub)
	return nr, nil
}

// split divides subtree r (which does not contain key) into the trees of keys below and
// above key.
func (t *Tree[V]) split(r *ref[V], b bounds, key string) (*ref[V], *ref[V], error) {
	if r == nil {
		return nil, nil, nil
	}
	n, err := t.load(r, b)
	if err != nil {
		return nil, nil, err
	}
	i := n.search(key)
	cl, cr, err := t.split(n.gap(i), n.gapBounds(b, i), key)
	if err != nil {
		return nil, nil, err
	}
	left, right := cl, cr
	if i > 0 {
		ln := &node[V]{layer: n.layer, left: n.left, ents: append([]ent[V](nil), n.ents[:i]...)}
		ln.ents[i-1].right = cl
		left = &ref[V]{n: ln}
	}
	if i < len(n.ents) {
		right = &ref[V]{n: &node[V]{layer: n.layer, left: cr, ents: append([]ent[V](nil), n.ents[i:]...)}}
	}
	return left, right, nil
}

// Delete removes key and reports whether it was present.
func (t *Tree[V]) Delete(key string) (bool, error) {
	r, found, err := t.delete(t.root, top, key)
	if err != nil || !found {
		return false, err
	}
	t.root = r
	return true, nil
}

func (t *Tree[V]) delete(r *ref[V], b bounds, key string) (*ref[V], bool, error) {
	if r == nil {
		return nil, false, nil
	}
	n, err := t.load(r, b)
	if err != nil {
		return nil, false, err
	}
	i := n.search(key)
	if i < len(n.ents) && n.ents[i].key == key {
		m, err := t.merge(n.gap(i), n.gapBounds(b, i), n.gap(i+1), n.gapBounds(b, i+1))
		if err != nil {
			return nil, false, err
		}
		if len(n.ents) == 1 {
			return m, true, nil
		}
		c := &node[V]{layer: n.layer, left: n.left}
		c.ents = append(append([]ent[V](nil), n.ents[:i]...), n.ents[i+1:]...)
		c.setGap(i, m)
		return &ref[V]{n: c}, true, nil
	}
	sub, found, err := t.delete(n.gap(i), n.gapBounds(b, i), key)
	if err != nil || !found {
		return r, false, err
	}
	c, nr := mut(r)
	c.setGap(i, sub)
	return nr, true, nil
}

// merge joins adjacent subtrees a < b.
func (t *Tree[V]) merge(a *ref[V], ba bounds, b *ref[V], bb bounds) (*ref[V], error) {
	if a == nil {
		return b, nil
	}
	if b == nil {
		return a, nil
	}
	na, err := t.load(a, ba)
	if err != nil {
		return nil, err
	}
	nb, err := t.load(b, bb)
	if err != nil {
		return nil, err
	}
	la, lb := len(na.ents), 0
	switch {
	case na.layer > nb.layer:
		m, err := t.merge(na.gap(la), na.gapBounds(ba, la), b, bb)
		if err != nil {
			return nil, err
		}
		c, nr := mut(a)
		c.setGap(la, m)
		return nr, nil
	case nb.layer > na.layer:
		m, err := t.merge(a, ba, nb.gap(lb), nb.gapBounds(bb, lb))
		if err != nil {
			return nil, err
		}
		c, nr := mut(b)
		c.setGap(0, m)
		return nr, nil
	}
	m, err := t.merge(na.gap(la), na.gapBounds(ba, la), nb.gap(0), nb.gapBounds(bb, 0))
	if err != nil {
		return nil, err
	}
	c := &node[V]{layer: na.layer, left: na.left, ents: append(append([]ent[V](nil), na.ents...), nb.ents...)}
	c.ents[la-1].right = m
	return &ref[V]{n: c}, nil
}

// Flush writes every new node and returns the root CID. The empty tree is a node with no
// entries.
func (t *Tree[V]) Flush() (cid.CID, error) {
	if t.root == nil {
		t.root = &ref[V]{n: &node[V]{}}
	}
	if err := t.persist(t.root); err != nil {
		return cid.CID{}, err
	}
	return t.root.c, nil
}

func (t *Tree[V]) persist(r *ref[V]) error {
	if r == nil || r.c.Defined() {
		return nil
	}
	n := r.n
	w := &Node[V]{Entries: make([]Entry[V], len(n.ents))}
	if err := t.persist(n.left); err != nil {
		return err
	}
	if n.left != nil {
		w.Left = n.left.c
	}
	for i, e := range n.ents {
		if err := t.persist(e.right); err != nil {
			return err
		}
		w.Entries[i] = Entry[V]{Key: e.key, Val: e.val}
		if e.right != nil {
			w.Entries[i].Right = e.right.c
		}
	}
	data, err := t.p.Codec.Encode(w)
	if err != nil {
		return err
	}
	if len(data) > t.p.MaxNodeSize {
		return fmt.Errorf("%w (%d bytes, %d entries)", ErrNodeTooLarge, len(data), len(n.ents))
	}
	c, err := cid.SumCID(cid.DRISL, cid.SHA256, data)
	if err != nil {
		return err
	}
	if err := t.st.Put(c, data); err != nil {
		return err
	}
	r.c = c
	return nil
}

// Walk calls fn for each key >= from in order until fn returns false or an error.
func (t *Tree[V]) Walk(from string, fn func(key string, val V) (bool, error)) error {
	_, err := t.walk(t.root, top, from, fn)
	return err
}

// WalkPrefix calls fn for each key with the given prefix, in order.
func (t *Tree[V]) WalkPrefix(prefix string, fn func(key string, val V) (bool, error)) error {
	return t.Walk(prefix, func(k string, v V) (bool, error) {
		if !strings.HasPrefix(k, prefix) {
			return false, nil
		}
		return fn(k, v)
	})
}

func (t *Tree[V]) walk(r *ref[V], b bounds, from string, fn func(string, V) (bool, error)) (bool, error) {
	if r == nil {
		return true, nil
	}
	n, err := t.load(r, b)
	if err != nil {
		return false, err
	}
	for i := n.search(from); i <= len(n.ents); i++ {
		if ok, err := t.walk(n.gap(i), n.gapBounds(b, i), from, fn); !ok || err != nil {
			return false, err
		}
		if i < len(n.ents) {
			if ok, err := fn(n.ents[i].key, n.ents[i].val); !ok || err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// Prove returns the blocks on the search path of key, root first: an inclusion proof if
// key is present and an absence proof otherwise. The tree must be flushed.
func (t *Tree[V]) Prove(key string) ([][]byte, error) {
	var out [][]byte
	r, b := t.root, top
	for r != nil {
		if !r.c.Defined() {
			return nil, errors.New("mst: Prove needs a flushed tree")
		}
		data, err := t.st.Get(r.c)
		if err != nil {
			return nil, err
		}
		out = append(out, data)
		n, err := t.load(r, b)
		if err != nil {
			return nil, err
		}
		i := n.search(key)
		if i < len(n.ents) && n.ents[i].key == key {
			break
		}
		r, b = n.gap(i), n.gapBounds(b, i)
	}
	return out, nil
}

// VerifyProof checks a proof from Prove against root and returns key's value if present.
// Missing blocks or invalid nodes on the path are errors.
func (p *Profile[V]) VerifyProof(root cid.CID, key string, blocks [][]byte) (V, bool, error) {
	ms := MemStore{}
	for _, b := range blocks {
		c, err := cid.SumCID(cid.DRISL, cid.SHA256, b)
		if err != nil {
			var zero V
			return zero, false, err
		}
		ms[c] = b
	}
	return p.Load(ms, root).Get(key)
}

// Change is one difference reported by Diff: Old is nil for additions, New for removals.
type Change[V any] struct {
	Key      string
	Old, New *V
}

type tok[V any] struct {
	sub *ref[V]
	b   bounds
	e   *ent[V]
}

type cursor[V any] struct {
	t     *Tree[V]
	stack []tok[V] // next token on top
}

func (c *cursor[V]) peek() *tok[V] {
	if len(c.stack) == 0 {
		return nil
	}
	return &c.stack[len(c.stack)-1]
}

func (c *cursor[V]) pop() { c.stack = c.stack[:len(c.stack)-1] }

func (c *cursor[V]) expand() error {
	tk := *c.peek()
	c.pop()
	n, err := c.t.load(tk.sub, tk.b)
	if err != nil {
		return err
	}
	for i := len(n.ents); i >= 0; i-- {
		if g := n.gap(i); g != nil {
			c.stack = append(c.stack, tok[V]{sub: g, b: n.gapBounds(tk.b, i)})
		}
		if i > 0 {
			c.stack = append(c.stack, tok[V]{e: &n.ents[i-1]})
		}
	}
	return nil
}

func (c *cursor[V]) layer(tk *tok[V]) (int, error) {
	n, err := c.t.load(tk.sub, tk.b)
	if err != nil {
		return 0, err
	}
	return n.layer, nil
}

func sameRef[V any](a, b *ref[V]) bool { return a == b || (a.c.Defined() && a.c == b.c) }

// Diff reports the changes from tree a to tree b in key order, skipping subtrees the two
// trees share.
func Diff[V any](a, b *Tree[V], fn func(Change[V]) error) error {
	ca, cb := &cursor[V]{t: a}, &cursor[V]{t: b}
	if a.root != nil {
		ca.stack = []tok[V]{{sub: a.root, b: top}}
	}
	if b.root != nil {
		cb.stack = []tok[V]{{sub: b.root, b: top}}
	}
	for {
		x, y := ca.peek(), cb.peek()
		switch {
		case x == nil && y == nil:
			return nil
		case x != nil && y != nil && x.sub != nil && y.sub != nil:
			if sameRef(x.sub, y.sub) {
				ca.pop()
				cb.pop()
				continue
			}
			lx, err := ca.layer(x)
			if err != nil {
				return err
			}
			ly, err := cb.layer(y)
			if err != nil {
				return err
			}
			if lx >= ly {
				if err := ca.expand(); err != nil {
					return err
				}
			}
			if ly >= lx {
				if err := cb.expand(); err != nil {
					return err
				}
			}
			continue
		case x != nil && x.sub != nil:
			if err := ca.expand(); err != nil {
				return err
			}
			continue
		case y != nil && y.sub != nil:
			if err := cb.expand(); err != nil {
				return err
			}
			continue
		}
		var ch Change[V]
		switch {
		case y == nil || (x != nil && x.e.key < y.e.key):
			ch = Change[V]{Key: x.e.key, Old: &x.e.val}
			ca.pop()
		case x == nil || y.e.key < x.e.key:
			ch = Change[V]{Key: y.e.key, New: &y.e.val}
			cb.pop()
		default:
			ca.pop()
			cb.pop()
			if a.p.Equal != nil && a.p.Equal(x.e.val, y.e.val) {
				continue
			}
			ch = Change[V]{Key: x.e.key, Old: &x.e.val, New: &y.e.val}
		}
		if err := fn(ch); err != nil {
			return err
		}
	}
}

// MemStore is an in-memory Store.
type MemStore map[cid.CID][]byte

func (m MemStore) Get(c cid.CID) ([]byte, error) {
	if b, ok := m[c]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, c)
}

func (m MemStore) Put(c cid.CID, data []byte) error {
	m[c] = data
	return nil
}
