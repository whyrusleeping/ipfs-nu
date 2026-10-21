package tree

import (
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
)

// Collections (D22): an item is an entry that carries `meta`; entries below an item belong
// to it. A container is a directory without `meta`, which a catalog walk descends into.
// Leaves are the units a union merge decides on: items, plus plain files and symlinks
// outside any item.

// IsContainer reports whether e is a directory without metadata (part of the catalog
// structure, not an item).
func IsContainer(e Entry) bool { return e.Kind == KindDir && !e.Meta.Defined() }

// CatalogProfile is the name of the catalog walk profile.
const CatalogProfile = "nu-catalog"

// CatalogLinks is the nu-catalog walk profile: the nu-tree profile, except that for an
// entry with `meta` only the metadata record is followed (not the item's target), and
// plain files and symlinks outside items are not followed. It reaches the catalog: the
// header, container directory MSTs and every item's metadata record.
func CatalogLinks(c cid.CID, data []byte) ([]cid.CID, error) {
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
	if m["$type"] != nil {
		if m["$type"] == TypeHeader {
			return Links(c, data)
		}
		return nil, nil
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
		switch {
		case e.Val.Meta.Defined():
			out = append(out, e.Val.Meta)
		case e.Val.Kind == KindDir:
			out = append(out, e.Val.CID)
		}
		if e.Right.Defined() {
			out = append(out, e.Right)
		}
	}
	return out, nil
}
