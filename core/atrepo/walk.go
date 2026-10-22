package atrepo

import (
	"slices"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
)

// Links is the atrepo walk profile (commit → MST → records). Blocks are classified by
// shape: a v3 commit links to its data; an MST node to its subtrees and records (in
// streamable order); anything else is a record, whose links (other repos' records, blobs)
// are not followed and so never make a pin incomplete.
func Links(c cid.CID, data []byte) ([]cid.CID, error) { return links(c, data, false) }

// LinksWithBlobs is the atrepo profile with blobs selected: records also link to the
// blobs they reference ({$type: "blob", ref} and legacy {cid, mimeType} refs).
func LinksWithBlobs(c cid.CID, data []byte) ([]cid.CID, error) { return links(c, data, true) }

func links(c cid.CID, data []byte, blobs bool) ([]cid.CID, error) {
	if c.Codec() != cid.DRISL {
		return nil, nil
	}
	if com, err := DecodeCommit(data); err == nil {
		return []cid.CID{com.Data}, nil
	}
	if l, ok := nodeLinks(data); ok {
		return l, nil
	}
	if !blobs {
		return nil, nil
	}
	v, err := drisl.AtprotoRecord.Decode(data)
	if err != nil {
		return nil, nil // not a valid record: nothing to follow
	}
	var out []cid.CID
	blobRefs(v, &out, 0)
	return out, nil
}

// RecordBlobs returns the blob refs of a record block (nil when it is not a valid
// record): the atrepo+blobs profile's links of a record.
func RecordBlobs(data []byte) []cid.CID {
	v, err := drisl.AtprotoRecord.Decode(data)
	if err != nil {
		return nil
	}
	var out []cid.CID
	blobRefs(v, &out, 0)
	return out
}

func blobRefs(v any, out *[]cid.CID, depth int) {
	if depth > drisl.DefaultMaxDepth {
		return
	}
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			blobRefs(e, out, depth+1)
		}
	case map[string]any:
		if x["$type"] == "blob" {
			if r, ok := x["ref"].(cid.CID); ok && r.Codec() == cid.Raw {
				*out = append(*out, r)
			}
			return
		}
		if s, ok := x["cid"].(string); ok && len(x) == 2 {
			if _, ok := x["mimeType"].(string); ok {
				if r, err := cid.Parse(s); err == nil && r.Codec() == cid.Raw {
					*out = append(*out, r)
				}
				return
			}
		}
		for _, k := range sortedKeys(x) {
			blobRefs(x[k], out, depth+1)
		}
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
