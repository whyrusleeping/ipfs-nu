package collection_test

import (
	"context"
	"encoding/json"
	"testing"
	"testing/fstest"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/collection"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/nutest"
)

func init() { node.ClusterInterval = 100 * time.Millisecond }

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), nutest.Timeout)
	t.Cleanup(cancel)
	return c
}

func join(t *testing.T, admin, nd *nutest.Node) {
	t.Helper()
	inv, err := admin.Local.ClusterInvite(ctx(t), apiv0.ClusterInviteRequest{Peer: nd.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nd.Local.ClusterJoin(ctx(t), apiv0.ClusterJoinRequest{Token: inv.Token}); err != nil {
		t.Fatal(err)
	}
}

// scene stores an item on nd (one file plus a STAC-like metadata record) and queues it
// into nd's writer head.
func scene(t *testing.T, nd *nutest.Node, path, id string, bbox []any, cloud float64) {
	t.Helper()
	st := nd.Local.Store
	res, err := tree.BuildFS(fstest.MapFS{"visual.tif": {Data: []byte("pixels of " + id)}}, ".", tree.BuildOptions{Blobs: st, Nodes: st})
	if err != nil {
		t.Fatal(err)
	}
	mc, mb, _ := drisl.Sum(map[string]any{"stac": map[string]any{"id": id, "bbox": bbox,
		"properties": map[string]any{"datetime": "2026-10-20T10:00:00Z", "eo:cloud_cover": cloud}}})
	if err := st.Put(mc, mb); err != nil {
		t.Fatal(err)
	}
	if err := nd.Local.Collections().Put("s2", path, &tree.Entry{Kind: tree.KindDir, CID: res.Root, Size: res.Bytes, Meta: mc}); err != nil {
		t.Fatal(err)
	}
}

func status(nd *nutest.Node) *apiv0.CollectionStatus {
	st, err := nd.Local.CollectionStatus(context.Background(), "s2")
	if err != nil {
		return &apiv0.CollectionStatus{}
	}
	return st
}

var (
	alps    = []any{6.0, 46.0, 7.0, 47.0}
	sea     = []any{-30.0, 30.0, -29.0, 31.0}
	alpine  = json.RawMessage(`[{"bbox": {"intersects": [5.9, 45.8, 10.5, 47.8]}, "cloud": {"max": 30}}]`)
	rescue2 = json.RawMessage(`[{"rescue": {"below": 2, "max_bytes": "1GiB"}}]`)
)

// TestCollectionCluster: two union writers on a private cluster without multicast; a
// regional/cloud rule on a third member fetches exactly its items from the writers; a
// rescue member running the index brings single-copy items to two copies; every member
// converges on the same merged root, including one that joins late and one that restarts.
func TestCollectionCluster(t *testing.T) {
	n := nutest.New(t, 4)
	a, b, c, d := n.Nodes[0], n.Nodes[1], n.Nodes[2], n.Nodes[3]
	if _, err := a.Local.ClusterInit(ctx(t), apiv0.ClusterInitRequest{Name: "collgs", Mode: cluster.ModePrivate}); err != nil {
		t.Fatal(err)
	}
	join(t, a, b)
	join(t, a, c)
	wa, err := a.Local.NetCluster().NewKey()
	if err != nil {
		t.Fatal(err)
	}
	wb, err := b.Local.NetCluster().NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Local.NetCluster().Write(
		cluster.Op{Collection: cluster.NSKey, RKey: wa, Value: &cluster.Key{Role: cluster.RoleWriter}},
		cluster.Op{Collection: cluster.NSKey, RKey: wb, Value: &cluster.Key{Role: cluster.RoleWriter}}); err != nil {
		t.Fatal(err)
	}
	ds, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "s2", Collection: true, Writers: []string{wa, wb},
		Facets: map[string]apiv0.FacetDecl{"bbox": {Ptr: "/stac/bbox", Type: "bbox"}, "cloud": {Ptr: "/stac/properties/eo:cloud_cover", Type: "number"},
			"sensed": {Ptr: "/stac/properties/datetime", Type: "time"}}})
	if err != nil {
		t.Fatal(err)
	}
	if ds.Replicas != 0 || ds.Collection == nil || ds.Collection.Merge != "union" {
		t.Fatalf("dataset %+v", ds)
	}
	nutest.Eventually(t, "dataset on b and c", func() bool {
		_, okb := b.Local.NetCluster().View().Dataset("s2")
		_, okc := c.Local.NetCluster().View().Dataset("s2")
		return okb && okc && b.Local.NetCluster().View().Keys[wb] != nil
	})

	scene(t, a, "2026/10/20/clear", "clear", alps, 10)
	scene(t, a, "2026/10/20/cloudy", "cloudy", alps, 80)
	scene(t, a, "2026/10/20/a-only", "a-only", alps, 20)
	scene(t, b, "2026/10/20/clear", "clear", alps, 10) // the same item pushed to both writers
	scene(t, b, "2026/10/20/sea", "sea", sea, 0)
	scene(t, b, "2026/10/20/b-only", "b-only", alps, 5)
	for _, nd := range []*nutest.Node{a, b} {
		if err := nd.Local.Collections().Flush(ctx(t), "s2"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Local.CollectionSetRules(ctx(t), "s2", alpine, false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Local.CollectionSetRules(ctx(t), "s2", rescue2, false); err != nil {
		t.Fatal(err)
	}

	converged := func(nodes ...*nutest.Node) bool {
		root := status(a).Root
		for _, nd := range nodes {
			if st := status(nd); st.Root != root || st.Items != 5 || st.Conflicts != 0 || len(st.Writers) != 2 {
				return false
			}
		}
		return root != ""
	}
	held := func(nd *nutest.Node, want ...string) bool {
		p, err := nd.Local.CollectionItems(context.Background(), "s2", apiv0.CollectionItemsQuery{Held: true})
		if err != nil || len(p.Items) != len(want) {
			return false
		}
		for i, it := range p.Items {
			if it.Path != want[i] || it.Local != collection.StateComplete {
				return false
			}
		}
		return true
	}
	nutest.Eventually(t, "union catalog on every member", func() bool { return converged(a, b, c) })
	nutest.Eventually(t, "alpine selection on c", func() bool {
		return held(c, "2026/10/20/a-only", "2026/10/20/b-only", "2026/10/20/clear")
	})
	nutest.Eventually(t, "rescue of the single-copy item by a", func() bool {
		it, err := a.Local.CollectionItem(context.Background(), "s2", "2026/10/20/sea")
		return err == nil && it.Local == collection.StateComplete && it.Copies != nil && *it.Copies == 2
	})
	nutest.Eventually(t, "rarest first from the index", func() bool {
		p, err := a.Local.CollectionItems(context.Background(), "s2", apiv0.CollectionItemsQuery{Rarest: true})
		return err == nil && len(p.Items) == 5 && *p.Items[0].Copies == 1 && p.Items[0].Path == "2026/10/20/cloudy" && *p.Items[4].Copies == 3
	})

	// A member that joins late converges on the same catalog and holdings.
	join(t, a, d)
	if _, err := d.Local.CollectionSetRules(ctx(t), "s2", alpine, false); err != nil {
		t.Fatal(err)
	}
	nutest.Eventually(t, "late joiner", func() bool {
		return converged(d) && held(d, "2026/10/20/a-only", "2026/10/20/b-only", "2026/10/20/clear")
	})
	// A restarted member keeps its holdings, and the index keeps counting them.
	c.Restart()
	nutest.Eventually(t, "restarted member", func() bool {
		it, err := a.Local.CollectionItem(context.Background(), "s2", "2026/10/20/b-only")
		return converged(c) && held(c, "2026/10/20/a-only", "2026/10/20/b-only", "2026/10/20/clear") &&
			err == nil && it.Copies != nil && *it.Copies == 3
	})
}
