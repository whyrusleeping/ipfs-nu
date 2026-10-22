package nutest

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/store"
)

// Item is one item of a test collection (BuildCollection): a directory with one file
// and a STAC-like metadata record `{stac: {id, bbox, properties: {datetime,
// eo:cloud_cover}}}`.
type Item struct {
	Path  string
	BBox  []any
	Cloud float64
	When  time.Time
}

// STACFacets are the facet declarations BuildCollection writes into the header.
var STACFacets = map[string]any{
	"sensed": map[string]any{"ptr": "/stac/properties/datetime", "type": "time"},
	"bbox":   map[string]any{"ptr": "/stac/bbox", "type": "bbox"},
	"cloud":  map[string]any{"ptr": "/stac/properties/eo:cloud_cover", "type": "number"},
}

// BuildCollection stores a flat collection tree (items as top-level entries, each with
// a metadata record) in st and returns the published header, which carries the facet
// declarations as a merged D22 header does, and each item's entry CID by path.
func BuildCollection(t testing.TB, st *store.Store, items []Item) (cid.CID, map[string]cid.CID) {
	t.Helper()
	dir := tree.Dir(st, cid.CID{})
	entries := map[string]cid.CID{}
	var total uint64
	for _, it := range items {
		res, err := tree.BuildFS(fstest.MapFS{"visual.tif": {Data: []byte(strings.Repeat(it.Path+" ", 2000))}}, ".", tree.BuildOptions{Blobs: st, Nodes: st})
		if err != nil {
			t.Fatal(err)
		}
		mc, mb, err := drisl.Sum(map[string]any{"stac": map[string]any{"id": it.Path, "bbox": it.BBox,
			"properties": map[string]any{"datetime": it.When.UTC().Format(time.RFC3339), "eo:cloud_cover": it.Cloud}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Put(mc, mb); err != nil {
			t.Fatal(err)
		}
		if err := dir.Put(it.Path, tree.Entry{Kind: tree.KindDir, CID: res.Root, Size: res.Bytes, Meta: mc}); err != nil {
			t.Fatal(err)
		}
		entries[it.Path] = res.Root
		total += res.Bytes
	}
	root, err := dir.Flush()
	if err != nil {
		t.Fatal(err)
	}
	hc, hb, err := drisl.Sum(tree.Header{V: 1, Root: root, Bytes: total, Collection: map[string]any{"facets": STACFacets}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(hc, hb); err != nil {
		t.Fatal(err)
	}
	if err := st.Pin(store.Pin{CID: hc, Mode: store.Recursive, Profile: "nu-tree", Source: store.SourceExplicit, Name: "collection"}); err != nil {
		t.Fatal(err)
	}
	return hc, entries
}
