// Package atrepo is the atproto repository profile (D21): v3 commits signed with P-256 or
// secp256k1 keys, the byte-exact atproto MST on core/mst, record proofs, repo diffs ("CAR
// slices") with operation inversion, full-repo verification, streamable CAR export and the
// atrepo walk profile. It does no I/O beyond the readers and writers it is handed.
package atrepo

import (
	"errors"
	"fmt"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
)

// Structural limits for untrusted repositories.
const (
	// MaxKeyLen bounds MST keys (repo paths), in bytes.
	MaxKeyLen = 1024
	// MaxNodeEntries bounds entries per MST node. A node's entry count is roughly geometric
	// (P(n or more) ≈ (3/4)^n), so honest trees never reach it (the largest seen in real
	// repos was 67); key-mined trees that pile keys into one node are refused.
	MaxNodeEntries = 256
	// MaxLayer bounds key layers. Reaching it needs a key hash with 64 leading zero bits.
	MaxLayer = 32
	// MaxRecordSize bounds one record block, in bytes.
	MaxRecordSize = 1_000_000
	// DefaultMaxBlocks bounds the blocks of one CAR.
	DefaultMaxBlocks = 1 << 24
)

// MST is the atproto v3 MST profile: SHA-256 key depth in 2-bit units (fanout 4), nodes
// {l, e: [{p, k, v, t}]} with mandatory prefix compression, empty intermediate nodes for
// skipped layers, and record CIDs as values.
var MST = &mst.Profile[cid.CID]{
	Name:        "atproto",
	LevelBits:   2,
	MaxNodeSize: drisl.MaxNodeSize,
	Codec:       nodeCodec{},
	ValidKey:    ValidMSTKey,
	Equal:       func(a, b cid.CID) bool { return a == b },
	Layered:     true,
	MaxEntries:  MaxNodeEntries,
	MaxLayer:    MaxLayer,
}

// ValidMSTKey accepts the keys atproto MSTs may hold: 1 to MaxKeyLen bytes. Repo paths
// written by Nu are further checked by ValidPath.
func ValidMSTKey(k string) error {
	if len(k) == 0 || len(k) > MaxKeyLen {
		return fmt.Errorf("atrepo: MST key length %d out of range", len(k))
	}
	return nil
}

// LoadTree opens the MST with root data.
func LoadTree(st mst.Store, data cid.CID) *mst.Tree[cid.CID] { return MST.Load(st, data) }

type nodeCodec struct{}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func optLink(c cid.CID) any {
	if !c.Defined() {
		return nil
	}
	return c
}

func (nodeCodec) Encode(n *mst.Node[cid.CID]) ([]byte, error) {
	ents := make([]any, len(n.Entries))
	prev := ""
	for i, e := range n.Entries {
		p := commonPrefix(prev, e.Key)
		ents[i] = map[string]any{
			"p": int64(p),
			"k": []byte(e.Key[p:]),
			"v": e.Val,
			"t": optLink(e.Right),
		}
		prev = e.Key
	}
	return drisl.Marshal(map[string]any{"e": ents, "l": optLink(n.Left)})
}

var errNode = errors.New("atrepo: malformed MST node")

// treeLink checks an MST child link: null or a SHA-256 DRISL CID.
func treeLink(v any) (cid.CID, error) {
	switch x := v.(type) {
	case nil:
		return cid.CID{}, nil
	case cid.CID:
		if x.Codec() != cid.DRISL || x.Hash() != cid.SHA256 {
			return cid.CID{}, fmt.Errorf("%w: subtree link %s is not a SHA-256 DRISL CID", errNode, x)
		}
		return x, nil
	}
	return cid.CID{}, fmt.Errorf("%w: subtree link is %T", errNode, v)
}

func (nodeCodec) Decode(data []byte) (*mst.Node[cid.CID], error) {
	v, err := drisl.DecOptions{MaxSize: drisl.MaxNodeSize, MaxDepth: 4, Atproto: true}.Decode(data)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) != 2 {
		return nil, fmt.Errorf("%w: want a map {e, l}", errNode)
	}
	es, ok := m["e"].([]any)
	if !ok {
		return nil, fmt.Errorf("%w: missing e", errNode)
	}
	lv, ok := m["l"]
	if !ok {
		return nil, fmt.Errorf("%w: missing l", errNode)
	}
	n := &mst.Node[cid.CID]{Entries: make([]mst.Entry[cid.CID], len(es))}
	if n.Left, err = treeLink(lv); err != nil {
		return nil, err
	}
	prev := ""
	for i, x := range es {
		em, ok := x.(map[string]any)
		if !ok || len(em) != 4 {
			return nil, fmt.Errorf("%w: entry %d: want a map {p, k, v, t}", errNode, i)
		}
		p, ok1 := em["p"].(int64)
		k, ok2 := em["k"].([]byte)
		val, ok3 := em["v"].(cid.CID)
		tv, ok4 := em["t"]
		if !ok1 || !ok2 || !ok3 || !ok4 {
			return nil, fmt.Errorf("%w: entry %d: bad field types", errNode, i)
		}
		if p < 0 || p > int64(len(prev)) {
			return nil, fmt.Errorf("%w: entry %d: prefix length %d out of range", errNode, i, p)
		}
		key := prev[:p] + string(k)
		if commonPrefix(prev, key) != int(p) {
			return nil, fmt.Errorf("%w: entry %d: prefix not maximally compressed", errNode, i)
		}
		e := &n.Entries[i]
		e.Key, e.Val = key, val
		if e.Right, err = treeLink(tv); err != nil {
			return nil, err
		}
		prev = key
	}
	return n, nil
}

// nodeLinks returns the links of an encoded MST node in streamable order: l, then v and t
// of each entry. ok is false if data is not an MST node.
func nodeLinks(data []byte) (links []cid.CID, ok bool) {
	n, err := nodeCodec{}.Decode(data)
	if err != nil {
		return nil, false
	}
	if n.Left.Defined() {
		links = append(links, n.Left)
	}
	for _, e := range n.Entries {
		links = append(links, e.Val)
		if e.Right.Defined() {
			links = append(links, e.Right)
		}
	}
	return links, true
}
