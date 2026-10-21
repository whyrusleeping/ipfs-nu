package mst

import (
	"bytes"
	"errors"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
)

// InlineCodec is the node schema of profiles with inline values and uncompressed keys
// (the Nu directory profile):
//
//	{ e: [ { k: <key string>, v: <value>, t?: <right subtree cid> } … ], l?: <left subtree cid> }
//
// Values are encoded with the drisl struct mapping. Decoding accepts only canonical bytes
// (the exact re-encoding), so unknown keys are rejected.
type InlineCodec[V any] struct{}

type wireNode[V any] struct {
	E []wireEnt[V] `drisl:"e"`
	L cid.CID      `drisl:"l,omitempty"`
}

type wireEnt[V any] struct {
	K string  `drisl:"k"`
	T cid.CID `drisl:"t,omitempty"`
	V V       `drisl:"v"`
}

func (InlineCodec[V]) Encode(n *Node[V]) ([]byte, error) {
	w := wireNode[V]{E: make([]wireEnt[V], len(n.Entries)), L: n.Left}
	for i, e := range n.Entries {
		w.E[i] = wireEnt[V]{K: e.Key, T: e.Right, V: e.Val}
	}
	return drisl.Marshal(w)
}

func (c InlineCodec[V]) Decode(data []byte) (*Node[V], error) {
	var w wireNode[V]
	if err := drisl.Unmarshal(data, &w); err != nil {
		return nil, err
	}
	n := &Node[V]{Left: w.L, Entries: make([]Entry[V], len(w.E))}
	for i, e := range w.E {
		n.Entries[i] = Entry[V]{Key: e.K, Val: e.V, Right: e.T}
	}
	re, err := c.Encode(n)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(re, data) {
		return nil, errors.New("non-canonical node (unknown or missing fields)")
	}
	return n, nil
}
