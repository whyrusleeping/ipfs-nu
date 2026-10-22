package collection_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/collection"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/nutest"
)

// TestCollectionFloor: a collection with replicas 2 places whole items by the hash of
// the item root (docs/collections.md "Guaranteed copies"): each item's two floor owners
// fetch it whole (src floor), `where` names the owners, the index counts the copies, and
// the collection is ready at its merged root once every item has two confirmed copies.
// A member joining changes the epoch: items move to it, and a previous owner keeps its
// complete copy as surplus.
func TestCollectionFloor(t *testing.T) {
	n := nutest.New(t, 4)
	a, b, c, d := n.Nodes[0], n.Nodes[1], n.Nodes[2], n.Nodes[3]
	if _, err := a.Local.ClusterInit(ctx(t), apiv0.ClusterInitRequest{Name: "floor", Mode: cluster.ModePrivate}); err != nil {
		t.Fatal(err)
	}
	join(t, a, b)
	join(t, a, c)
	if _, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "s2", Collection: true, Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	// A rescue rule that never triggers runs the holdings index on a.
	if _, err := a.Local.CollectionSetRules(ctx(t), "s2", json.RawMessage(`[{"rescue": {"below": 1, "max_bytes": "1GiB"}}]`), false); err != nil {
		t.Fatal(err)
	}
	const items = 6
	var paths []string
	for i := range items {
		p := fmt.Sprintf("2026/10/21/s%d", i)
		paths = append(paths, p)
		scene(t, a, p, p, alps, 1)
	}
	if err := a.Local.Collections().Flush(ctx(t), "s2"); err != nil {
		t.Fatal(err)
	}
	nodes := []*nutest.Node{a, b, c}
	owners := func(nd *nutest.Node, entry string) []string {
		v := nd.Local.NetCluster().View()
		ds, _ := v.Dataset("s2")
		e, _ := cid.Parse(entry)
		var out []string
		for _, id := range cluster.NewPlacement(v, ds.Policy, nd.ID).Owners(collection.ItemUnit(e)) {
			out = append(out, id.Base36())
		}
		return out
	}
	// placed reports whether every item is held complete by its two floor owners among
	// nodes; exact also requires that other members (but the writer a) do not hold it.
	placed := func(exact bool, nodes ...*nutest.Node) bool {
		for _, p := range paths {
			it, err := a.Local.CollectionItem(context.Background(), "s2", p)
			if err != nil || it.Entry == "" {
				return false
			}
			own := owners(a, it.Entry)
			if len(own) != 2 || !slices.Equal(it.Owners, own) {
				return false
			}
			for _, nd := range nodes {
				h, err := nd.Local.CollectionItem(context.Background(), "s2", p)
				if err != nil {
					return false
				}
				isOwner := slices.Contains(own, nd.ID.Base36())
				if (isOwner && h.Local != collection.StateComplete) || (exact && !isOwner && nd != a && h.Local != "") {
					return false
				}
			}
		}
		return true
	}
	nutest.Eventually(t, "floor owners hold their items", func() bool { return placed(true, nodes...) })
	// where: holders carry src floor; the index counts the floor copies.
	nutest.Eventually(t, "index counts floor copies", func() bool {
		for _, p := range paths {
			it, err := a.Local.CollectionItem(context.Background(), "s2", p)
			if err != nil || it.Copies == nil || *it.Copies < 2 {
				return false
			}
			floor := 0
			for _, h := range it.Holders {
				if h.Src == collection.SrcFloor && h.State == collection.StateComplete && h.Fresh {
					floor++
				}
			}
			if floor != 2 {
				return false
			}
		}
		return true
	})
	// Readiness at the merged root, items as units.
	root := status(a).Root
	w, err := b.Local.DatasetWait(ctx(t), node.ScopeCluster, "s2", apiv0.DatasetWaitRequest{Target: root, Timeout: "20s"})
	if err != nil || !w.Satisfied || w.Required != 2 || w.Copies == nil || w.Copies.Unit != "item" || w.Copies.Units != items || w.Copies.Below != 0 {
		t.Fatalf("collection ready wait: %+v, %v", w, err)
	}
	nutest.Eventually(t, "collection status ready on c", func() bool {
		ds, err := c.Local.Dataset(ctx(t), node.ScopeCluster, "s2")
		return err == nil && ds.Content.Target == root && ds.Content.State == cluster.ReadyReady
	})

	// A fourth member: the epoch changes, d acquires the items it now owns, and a member
	// that lost an item keeps its complete copy as surplus.
	join(t, a, d)
	nutest.Eventually(t, "items move to the new member", func() bool { return placed(false, a, b, c, d) })
	surplus := 0
	for _, nd := range []*nutest.Node{b, c} {
		if f := status(nd).Floor; f != nil {
			surplus += f.Surplus
		}
	}
	if surplus == 0 {
		t.Fatal("no previous owner kept a surplus copy after the epoch change")
	}
	w, err = d.Local.DatasetWait(ctx(t), node.ScopeCluster, "s2", apiv0.DatasetWaitRequest{Target: root, Timeout: "20s"})
	if err != nil || !w.Satisfied || w.Copies.Below != 0 {
		t.Fatalf("ready wait after the epoch change: %+v, %v", w, err)
	}
}
