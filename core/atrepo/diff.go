package atrepo

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/whyrusleeping/nu/core/car"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/mst"
)

// Op is one record operation between two revisions. Prev is zero for a create and CID is
// zero for a delete.
type Op struct {
	Path      string
	CID, Prev cid.CID
}

// Action returns "create", "update" or "delete".
func (o Op) Action() string {
	switch {
	case !o.Prev.Defined():
		return "create"
	case !o.CID.Defined():
		return "delete"
	}
	return "update"
}

var ErrInversion = errors.New("atrepo: operation inversion failed")

// Ops returns the record operations from tree prevData to tree data, in path order.
// prevData's tree is trusted; data's nodes are validated as they load.
func Ops(st Store, prevData, data cid.CID) ([]Op, error) {
	return opsBetween(LoadTree(st, prevData), LoadTree(st, data))
}

func opsBetween(a, b *mst.Tree[cid.CID]) ([]Op, error) {
	var ops []Op
	err := mst.Diff(a, b, func(ch mst.Change[cid.CID]) error {
		op := Op{Path: ch.Key}
		if ch.Old != nil {
			op.Prev = *ch.Old
		}
		if ch.New != nil {
			op.CID = *ch.New
		}
		ops = append(ops, op)
		return nil
	})
	return ops, err
}

// Invert applies ops in reverse to the (possibly partial) tree data in st and returns the
// resulting root, which equals the previous data when ops are complete and correct. Each op
// must match the tree: a create's or update's CID must be present at its path, a delete's
// path must be absent. Writes go to a scratch overlay, never to st. Missing nodes are errors
// wrapping mst.ErrNotFound.
func Invert(st Store, data cid.CID, ops []Op) (cid.CID, error) {
	tr := LoadTree(&overlay{top: mst.MemStore{}, base: st}, data)
	return invert(tr, ops)
}

func invert(tr *mst.Tree[cid.CID], ops []Op) (cid.CID, error) {
	sorted := slices.Clone(ops)
	slices.SortFunc(sorted, func(a, b Op) int { return strings.Compare(a.Path, b.Path) })
	for i, op := range sorted {
		if i > 0 && op.Path == sorted[i-1].Path {
			return cid.CID{}, fmt.Errorf("%w: two operations on %s", ErrInversion, op.Path)
		}
		if !op.CID.Defined() && !op.Prev.Defined() {
			return cid.CID{}, fmt.Errorf("%w: empty operation on %s", ErrInversion, op.Path)
		}
		cur, ok, err := tr.Get(op.Path)
		if err != nil {
			return cid.CID{}, err
		}
		if op.CID.Defined() {
			if !ok || cur != op.CID {
				return cid.CID{}, fmt.Errorf("%w: %s %s does not match the tree", ErrInversion, op.Action(), op.Path)
			}
		} else if ok {
			return cid.CID{}, fmt.Errorf("%w: deleted %s is still in the tree", ErrInversion, op.Path)
		}
		if op.Prev.Defined() {
			err = tr.Put(op.Path, op.Prev)
		} else {
			_, err = tr.Delete(op.Path)
		}
		if err != nil {
			return cid.CID{}, err
		}
	}
	return tr.Flush()
}

// Slice builds the CAR slice ("repo diff") from tree prevData to commit c: the commit
// first, every MST node of the new tree that the old one lacks, the nodes operation
// inversion needs, and the created or updated records. It also returns the ops, and checks
// that inverting them over the slice alone yields prevData.
func Slice(st Store, prevData, c cid.CID) ([]car.Block, []Op, error) {
	r, err := Open(st, c)
	if err != nil {
		return nil, nil, err
	}
	nodes, ops, err := sliceNodes(st, prevData, r.Head.Data)
	if err != nil {
		return nil, nil, err
	}
	commit, err := get(st, c)
	if err != nil {
		return nil, nil, err
	}
	out := []car.Block{commit}
	have := map[cid.CID]bool{c: true}
	add := func(id cid.CID) error {
		if have[id] {
			return nil
		}
		have[id] = true
		b, err := get(st, id)
		out = append(out, b)
		return err
	}
	for _, n := range nodes {
		if err := add(n); err != nil {
			return nil, nil, err
		}
	}
	for _, op := range ops {
		if op.CID.Defined() {
			if err := add(op.CID); err != nil {
				return nil, nil, err
			}
		}
	}
	return out, ops, nil
}

// sliceNodes returns the MST nodes a slice from prevData to data carries, and the ops.
func sliceNodes(st Store, prevData, data cid.CID) ([]cid.CID, []Op, error) {
	rec := &recorder{Store: st}
	ops, err := opsBetween(LoadTree(st, prevData), LoadTree(rec, data))
	if err != nil {
		return nil, nil, err
	}
	// The nodes inversion reads from the new tree.
	inv := &recorder{Store: st}
	root, err := invert(LoadTree(&overlay{top: mst.MemStore{}, base: inv}, data), ops)
	if err != nil {
		return nil, nil, err
	}
	if root != prevData {
		return nil, nil, fmt.Errorf("%w: inverted root %s, want %s", ErrInversion, root, prevData)
	}
	nodes := rec.got
	for _, n := range inv.got {
		if !rec.seen[n] {
			nodes = append(nodes, n)
		}
	}
	return nodes, ops, nil
}

// VerifyCommit checks a firehose-style commit: the CAR slice blocks in st (commit c), the
// commit under o, the ops by inversion to prevData, and the presence and validity of
// every created or updated record (hard errors only, as in Verify). It returns the commit.
func VerifyCommit(st Store, c cid.CID, ops []Op, prevData cid.CID, o VerifyOptions) (*Commit, error) {
	r, err := openVerified(st, c, o)
	if err != nil {
		return nil, err
	}
	for _, op := range ops {
		if !op.CID.Defined() {
			continue
		}
		data, err := st.Get(op.CID)
		if err != nil {
			return nil, fmt.Errorf("atrepo: record %s missing from the slice: %w", op.Path, err)
		}
		if _, err := checkRecord(data); err != nil {
			return nil, fmt.Errorf("%s: %w", op.Path, err)
		}
	}
	root, err := Invert(st, r.Head.Data, ops)
	if err != nil {
		return nil, err
	}
	if root != prevData {
		return nil, fmt.Errorf("%w: inverted root %s, want prevData %s", ErrInversion, root, prevData)
	}
	return r.Head, nil
}

// Applied is the result of applying a diff to a held repository.
type Applied struct {
	Commit    *Commit
	CommitCID cid.CID
	Ops       []Op
	// Blocks are the diff blocks the new revision uses (commit, new MST nodes, new
	// records); the caller stores them. Unused diff blocks are dropped.
	Blocks []car.Block
}

// ApplySince applies a getRepo?since=<rev> CAR (r) to the verified tree prevData held in
// held. It verifies the new commit under o, validates every MST node that differs from the
// held tree, and requires every created or updated record to be in the diff or held and
// valid, so the new data root is fully verified. Missing nodes (a broken chain) are errors
// wrapping mst.ErrNotFound; the caller then falls back to a full export.
func ApplySince(held Store, prevData cid.CID, r io.Reader, o VerifyOptions) (*Applied, error) {
	diff := mst.MemStore{}
	root, err := ReadCAR(r, diff, o.MaxBlocks)
	if err != nil {
		return nil, err
	}
	used := &recorder{Store: diff}
	both := &overlay{top: mst.MemStore{}, base: storeChain{used, held}}
	rp, err := openVerified(both, root, o)
	if err != nil {
		return nil, err
	}
	ops, err := opsBetween(LoadTree(held, prevData), LoadTree(both, rp.Head.Data))
	if err != nil {
		return nil, err
	}
	for _, op := range ops {
		if !op.CID.Defined() {
			continue
		}
		data, err := both.Get(op.CID)
		if err != nil {
			return nil, fmt.Errorf("atrepo: record %s: %w", op.Path, err)
		}
		if _, err := checkRecord(data); err != nil {
			return nil, fmt.Errorf("%s: %w", op.Path, err)
		}
	}
	a := &Applied{Commit: rp.Head, CommitCID: root, Ops: ops}
	for _, c := range used.got {
		a.Blocks = append(a.Blocks, car.Block{CID: c, Data: diff[c]})
	}
	return a, nil
}

// storeChain reads from the first store that has a block.
type storeChain [2]Store

func (s storeChain) Get(c cid.CID) ([]byte, error) {
	b, err := s[0].Get(c)
	if err == nil || !errors.Is(err, mst.ErrNotFound) {
		return b, err
	}
	return s[1].Get(c)
}

func (s storeChain) Put(c cid.CID, data []byte) error { return s[0].Put(c, data) }
