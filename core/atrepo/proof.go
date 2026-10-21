package atrepo

import (
	"errors"
	"fmt"
	"io"

	"github.com/whyrusleeping/nu/core/car"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
)

// MaxProofBlocks bounds the blocks of a record proof CAR.
const MaxProofBlocks = 512

// ReadCAR reads a repository CAR into st, verifying every block against its CID, and
// returns the first root (the commit). Blocks over 1 MiB and more than maxBlocks blocks
// (0 means DefaultMaxBlocks) are refused. Order and duplicates do not matter.
func ReadCAR(r io.Reader, st Store, maxBlocks int) (cid.CID, error) {
	if maxBlocks == 0 {
		maxBlocks = DefaultMaxBlocks
	}
	cr, err := car.NewReader(r, car.Options{MaxBlockSize: drisl.MaxNodeSize})
	if err != nil {
		return cid.CID{}, err
	}
	h := cr.Header()
	if len(h.Roots) == 0 {
		return cid.CID{}, errors.New("atrepo: CAR has no root commit")
	}
	for n := 0; ; n++ {
		b, dup, err := cr.Next()
		if err == io.EOF {
			return h.Roots[0], nil
		}
		if err != nil {
			return cid.CID{}, err
		}
		if n >= maxBlocks {
			return cid.CID{}, fmt.Errorf("%w: more than %d blocks", mst.ErrLimit, maxBlocks)
		}
		if !dup {
			if err := st.Put(b.CID, b.Data); err != nil {
				return cid.CID{}, err
			}
		}
	}
}

// WriteCAR writes blocks as a CAR with root first; the blocks are written in order.
func WriteCAR(w io.Writer, root cid.CID, blocks []car.Block) error {
	cw, err := car.NewWriter(w, car.Header{Roots: []cid.CID{root}})
	if err != nil {
		return err
	}
	for _, b := range blocks {
		if err := cw.Put(b); err != nil {
			return err
		}
	}
	return nil
}

// Export writes the repository at commit c in streamable order: the commit, then the MST
// in pre-order with each node followed by its slots in order (left subtree, then each
// entry's record and right subtree), every block once. Every block must be in st.
func Export(w io.Writer, st Store, c cid.CID) error {
	r, err := Open(st, c)
	if err != nil {
		return err
	}
	cw, err := car.NewWriter(w, car.Header{Roots: []cid.CID{c}})
	if err != nil {
		return err
	}
	seen := map[cid.CID]bool{}
	put := func(c cid.CID) ([]byte, error) {
		if seen[c] {
			return nil, nil
		}
		seen[c] = true
		b, err := get(st, c)
		if err != nil {
			return nil, err
		}
		return b.Data, cw.Put(b)
	}
	if _, err := put(c); err != nil {
		return err
	}
	var node func(c cid.CID, depth int) error
	node = func(c cid.CID, depth int) error {
		if depth > 2*MaxLayer+2 {
			return fmt.Errorf("%w: tree too deep", mst.ErrLimit)
		}
		data, err := put(c)
		if err != nil || data == nil {
			return err
		}
		n, err := nodeCodec{}.Decode(data)
		if err != nil {
			return fmt.Errorf("atrepo: node %s: %w", c, err)
		}
		if n.Left.Defined() {
			if err := node(n.Left, depth+1); err != nil {
				return err
			}
		}
		for _, e := range n.Entries {
			if _, err := put(e.Val); err != nil {
				return fmt.Errorf("atrepo: record %s: %w", e.Key, err)
			}
			if e.Right.Defined() {
				if err := node(e.Right, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return node(r.Head.Data, 0)
}

// Prove returns the blocks that prove path's presence or absence at commit c, as
// com.atproto.sync.getRecord serves them: the commit, the MST nodes on the path (including
// intermediate nodes) and, if present, the record.
func Prove(st Store, c cid.CID, path string) ([]car.Block, error) {
	r, err := Open(st, c)
	if err != nil {
		return nil, err
	}
	commit, err := get(st, c)
	if err != nil {
		return nil, err
	}
	out := []car.Block{commit}
	var last []byte
	if _, err := MST.Path(st, r.Head.Data, path, func(n cid.CID) {
		b, _ := get(st, n)
		out = append(out, b)
		last = b.Data
	}); err != nil {
		return nil, err
	}
	if last != nil {
		n, err := nodeCodec{}.Decode(last)
		if err != nil {
			return nil, err
		}
		for _, e := range n.Entries {
			if e.Key == path {
				rec, err := get(st, e.Val)
				if err != nil {
					return nil, err
				}
				out = append(out, rec)
			}
		}
	}
	return out, nil
}

// RecordProof is a verified record lookup.
type RecordProof struct {
	Commit    *Commit
	CommitCID cid.CID
	Path      string
	CID       cid.CID // zero when the proof shows absence
	Data      []byte  // the record block, nil when absent
	// DataModel is the record's atproto data-model violation, if any (it is still
	// returned; see Stats.NonConforming).
	DataModel error
}

// VerifyRecord checks a getRecord-style proof CAR for path: the commit under o, the MST
// path from data, and the record block. It returns the record or proven absence.
func VerifyRecord(r io.Reader, path string, o VerifyOptions) (*RecordProof, error) {
	st := mst.MemStore{}
	if o.MaxBlocks == 0 {
		o.MaxBlocks = MaxProofBlocks
	}
	root, err := ReadCAR(r, st, o.MaxBlocks)
	if err != nil {
		return nil, err
	}
	rp, err := openVerified(st, root, o)
	if err != nil {
		return nil, err
	}
	p := &RecordProof{Commit: rp.Head, CommitCID: root, Path: path}
	c, data, ok, err := rp.Get(path)
	if err != nil {
		return nil, err
	}
	if ok {
		if p.DataModel, err = checkRecord(data); err != nil {
			return nil, err
		}
		p.CID, p.Data = c, data
	}
	return p, nil
}
