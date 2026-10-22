package fetch

import (
	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
)

// AtrepoLinks is the layer-1 fallback and placement completeness walk. Records
// never follow arbitrary links. Selected blobs with a declared size over the
// whole-object protocol limit are excluded from placement obligations.
func AtrepoLinks(c cid.CID, data []byte, blobs bool) ([]Link, error) {
	refs, err := atrepo.Links(c, data)
	if err != nil {
		return nil, err
	}
	out := make([]Link, 0, len(refs))
	for _, c := range refs {
		out = append(out, Link{CID: c})
	}
	if len(refs) > 0 || !blobs || c.Codec() != cid.DRISL {
		return out, nil
	}
	sizes := map[cid.CID]uint64{}
	v, err := drisl.AtprotoRecord.Decode(data)
	if err != nil {
		return out, nil
	}
	var scan func(any)
	scan = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				scan(e)
			}
		case map[string]any:
			if x["$type"] == "blob" {
				if c, ok := x["ref"].(cid.CID); ok {
					sz, _ := x["size"].(int64)
					// Conflicting refs choose the smaller obligation; never let
					// a map's iteration order decide whether a blob is skipped.
					n := uint64(max(sz, 0))
					if prev, ok := sizes[c]; !ok || n < prev {
						sizes[c] = n
					}
				}
				return
			}
			if s, ok := x["cid"].(string); ok && len(x) == 2 {
				if _, ok := x["mimeType"].(string); ok {
					if c, err := cid.Parse(s); err == nil {
						sizes[c] = 0
					}
					return
				}
			}
			for _, e := range x {
				scan(e)
			}
		}
	}
	scan(v)
	for _, c := range atrepo.RecordBlobs(data) {
		sz := sizes[c]
		if sz > cid.MaxWholeObject {
			continue
		}
		out = append(out, Link{CID: c, Size: sz})
	}
	return out, nil
}
