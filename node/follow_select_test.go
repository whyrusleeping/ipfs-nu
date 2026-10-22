package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/nutest/atfix"
	"github.com/whyrusleeping/nu/store"
)

// catalogItem is one item of a test collection: a directory with one file and a
// STAC-like metadata record.
type catalogItem struct {
	path  string
	bbox  []any
	cloud float64
	when  time.Time
}

var testFacets = map[string]any{
	"sensed": map[string]any{"ptr": "/stac/properties/datetime", "type": "time"},
	"bbox":   map[string]any{"ptr": "/stac/bbox", "type": "bbox"},
	"cloud":  map[string]any{"ptr": "/stac/properties/eo:cloud_cover", "type": "number"},
}

// buildCollection stores a flat collection tree (items as top-level entries) in st and
// returns the published header (root plus the facet declarations, as a merged D22
// header carries them).
func buildCollection(t *testing.T, st *store.Store, items []catalogItem) cid.CID {
	t.Helper()
	dir := tree.Dir(st, cid.CID{})
	var total uint64
	for _, it := range items {
		res, err := tree.BuildFS(fstest.MapFS{"visual.tif": {Data: []byte(strings.Repeat(it.path, 64))}}, ".", tree.BuildOptions{Blobs: st, Nodes: st})
		if err != nil {
			t.Fatal(err)
		}
		mc, mb, err := drisl.Sum(map[string]any{"stac": map[string]any{"id": it.path, "bbox": it.bbox,
			"properties": map[string]any{"datetime": it.when.Format(time.RFC3339), "eo:cloud_cover": it.cloud}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Put(mc, mb); err != nil {
			t.Fatal(err)
		}
		if err := dir.Put(it.path, tree.Entry{Kind: tree.KindDir, CID: res.Root, Size: res.Bytes, Meta: mc}); err != nil {
			t.Fatal(err)
		}
		total += res.Bytes
	}
	root, err := dir.Flush()
	if err != nil {
		t.Fatal(err)
	}
	hc, hb, err := drisl.Sum(tree.Header{V: 1, Root: root, Bytes: total, Collection: map[string]any{"facets": testFacets}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(hc, hb); err != nil {
		t.Fatal(err)
	}
	return hc
}

var alpsBox, seaBox = []any{6.0, 46.0, 7.0, 47.0}, []any{-30.0, 30.0, -29.0, 31.0}

// `nu follow <name> --select rules` (0058): the follow is a select-mode collection
// dataset; its catalog is pinned whole and only the items the rules choose are held,
// with the facets read from the published header. Following again with new rules
// replaces them and re-evaluates without refetching held items; a newer publication
// converges on refresh; rescue rules and placement flags are refused; unfollow releases
// the catalog and item pins.
func TestFollowSelect(t *testing.T) {
	f := atfix.New(t)
	n := newLocal(t)
	n.Atproto = atproto.New(f.Config())
	ctx := context.Background()
	now := time.Now()
	items := []catalogItem{{"alps-clear", alpsBox, 10, now.Add(-time.Hour)}, {"alps-cloudy", alpsBox, 80, now.Add(-time.Hour)}, {"sea", seaBox, 0, now.Add(-time.Hour)}}
	hdr := buildCollection(t, n.Store, items)
	publish := func(h cid.CID, at string) {
		f.Pub.Put("io.ipfs.nu.name", "scenes", map[string]any{"$type": "io.ipfs.nu.name", "target": h.String(), "kind": "tree", "updatedAt": at})
	}
	publish(hdr, "2026-01-01T00:00:00Z")

	alps := json.RawMessage(`[{"bbox": {"intersects": [5.9, 45.8, 10.5, 47.8]}, "cloud": {"max": 30}}]`)
	for _, bad := range []apiv0.FollowRequest{
		{Target: atfix.PubDID + "/scenes", Select: json.RawMessage(`[{"rescue": {"below": 2}}]`)},
		{Target: atfix.PubDID + "/scenes", Select: alps, All: true},
		{Target: atfix.PubDID + "/scenes", Select: json.RawMessage(`[]`)},
	} {
		if _, err := n.Follow(ctx, bad); !isCode(err, apiv0.CodeInvalidArgument) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	fl, err := n.Follow(ctx, apiv0.FollowRequest{Target: atfix.PubDID + "/scenes", Dataset: "scenes", Select: alps})
	if err != nil {
		t.Fatal(err)
	}
	if fl.Mode != "select" || fl.Selection == nil || !fl.Selection.CatalogSynced || fl.Selection.Items != 3 || fl.Selection.Selected != 1 || fl.Selection.Held != 1 ||
		fl.Held == nil || fl.Held.Target != hdr.String() || fl.State != "held" || len(fl.Selection.Facets) != 3 {
		t.Fatalf("select follow: %+v %+v", fl, fl.Selection)
	}
	held := func() map[string]bool {
		pins, _ := n.Store.Pins()
		out := map[string]bool{}
		for _, p := range pins {
			if p.Source == store.SourceCollectionSelect {
				out[strings.TrimPrefix(p.Name, "scenes/")] = true
			}
		}
		return out
	}
	if h := held(); len(h) != 1 || !h["alps-clear"] {
		t.Fatalf("held %v", h)
	}
	// The catalog is held whole: every item is listed, but only a selected item's
	// content is held.
	if ents, err := n.Ls(ctx, "scenes"); err != nil || len(ents.Entries) != 3 {
		t.Fatalf("ls: %+v %v", ents, err)
	}
	st, err := n.CollectionStatus(ctx, "scenes")
	if err != nil || st.Items != 3 || st.Rules != 1 {
		t.Fatalf("collection status through the follow: %+v %v", st, err)
	}

	// New rules: following the same name again replaces them; alps-clear stays held
	// without a new transition.
	seq0 := st.LogSeq
	fl, err = n.Follow(ctx, apiv0.FollowRequest{Target: atfix.PubDID + "/scenes", Select: json.RawMessage(`[{"bbox": {"intersects": [5.9, 45.8, 10.5, 47.8]}}]`)})
	if err != nil || fl.Selection.Held != 2 {
		t.Fatalf("rules replaced: %+v %v", fl.Selection, err)
	}
	if h := held(); len(h) != 2 || !h["alps-cloudy"] {
		t.Fatalf("held %v", h)
	}
	page, _ := n.clusters.lcollPtr.Load().Holdings("", cluster.HoldingsRequest{Dataset: "scenes", Epoch: st.LogEpoch, Since: seq0})
	for _, e := range page.Entries {
		if e.Path == "alps-clear" {
			t.Fatalf("held item re-transitioned: %+v", e)
		}
	}

	// A newer publication adds an alpine item and drops the sea: refresh converges.
	items = append(items[:2], catalogItem{"alps-new", alpsBox, 5, now})
	hdr2 := buildCollection(t, n.Store, items)
	publish(hdr2, "2026-01-02T00:00:00Z")
	fls, err := n.FollowRefresh(ctx, apiv0.FollowRefreshRequest{Dataset: "scenes"})
	if err != nil || len(fls) != 1 || fls[0].Held.Target != hdr2.String() || fls[0].Selection.Items != 3 || fls[0].Selection.Held != 3 {
		t.Fatalf("after republish: %+v %v", fls, err)
	}
	// Re-following with a max_size updates the follow's budget (held items stay; the
	// budget pauses later matches, see collection.TestFollowSelection).
	fl, err = n.Follow(ctx, apiv0.FollowRequest{Target: atfix.PubDID + "/scenes", Select: json.RawMessage(`[{}]`), MaxSize: 1})
	if err != nil || fl.Selection.Held != 3 {
		t.Fatalf("re-follow with max_size: %+v %v", fl.Selection, err)
	}
	if d, _ := n.clusters.local.View().Dataset("scenes"); d.Source.Follow.MaxSize != 1 {
		t.Fatalf("max_size not updated: %+v", d.Source.Follow)
	}

	// Unfollow releases the follow's pins.
	uf, err := n.Unfollow(ctx, "scenes")
	if err != nil || len(uf.Orphans) < 4 {
		t.Fatalf("unfollow: %+v %v", uf, err)
	}
	pins, _ := n.Store.Pins()
	for _, p := range pins {
		if strings.HasPrefix(p.Name, "catalog/scenes") || strings.HasPrefix(p.Name, "scenes/") {
			t.Fatalf("pin left: %+v", p)
		}
	}
	if _, err := n.CollectionStatus(ctx, "scenes"); err == nil {
		t.Fatal("collection state left after unfollow")
	}
}
