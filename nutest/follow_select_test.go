package nutest_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/nutest"
	"github.com/whyrusleeping/nu/nutest/atfix"
)

var alps, sea = []any{6.0, 46.0, 7.0, 47.0}, []any{-30.0, 30.0, -29.0, 31.0}

// A select follow over the network (0058; docs/collections.md "Following collections"):
// the publisher a holds a collection and publishes its header under a name on the
// fixture PDS; b follows it with regional/rolling rules, fetches the catalog whole and
// only the selected items from a, keeps them across a restart without refetching, and
// converges on a publication it missed while stopped (the follow refresh re-resolves
// the name; the catalog diff adds the new item and releases the one that left).
func TestFollowSelectNetwork(t *testing.T) {
	old := node.ClusterInterval
	node.ClusterInterval = 200 * time.Millisecond
	defer func() { node.ClusterInterval = old }()
	f := atfix.New(t)
	n := nutest.New(t, 2)
	a, b := n.Nodes[0], n.Nodes[1]
	ctx := context.Background()
	b.Local.Atproto = atproto.New(f.Config())
	n.Peer(b, a)

	now := time.Now()
	items := []nutest.Item{{Path: "alps-clear", BBox: alps, Cloud: 10, When: now.Add(-time.Hour)}, {Path: "alps-cloudy", BBox: alps, Cloud: 80, When: now.Add(-time.Hour)},
		{Path: "sea", BBox: sea, Cloud: 0, When: now.Add(-time.Hour)}, {Path: "alps-old", BBox: alps, Cloud: 5, When: now.Add(-72 * time.Hour)}}
	hdr, entries := nutest.BuildCollection(t, a.Local.Store, items)
	publish := func(target, at string) {
		f.Pub.Put("io.ipfs.nu.name", "scenes", map[string]any{"$type": "io.ipfs.nu.name", "target": target, "kind": "tree", "updatedAt": at})
	}
	publish(hdr.String(), "2026-01-01T00:00:00Z")

	rules := json.RawMessage(`[{"bbox": {"intersects": [5.9, 45.8, 10.5, 47.8]}, "cloud": {"max": 30}, "sensed": {"within": "48h"}}]`)
	fl, err := b.Local.Follow(ctx, apiv0.FollowRequest{Target: atfix.PubDID + "/scenes", Dataset: "scenes", Select: rules})
	if err != nil || fl.Mode != "select" {
		t.Fatalf("follow: %+v %v", fl, err)
	}
	settled := func(held int, target string) func() bool {
		return func() bool {
			fs, _ := b.Local.Follows(ctx)
			return len(fs) == 1 && fs[0].Selection != nil && fs[0].Selection.CatalogSynced && fs[0].Selection.Held == held && fs[0].Selection.Fetching == 0 &&
				fs[0].Held != nil && fs[0].Held.Target == target && fs[0].State == "held"
		}
	}
	nutest.Eventually(t, "catalog synced and the alpine scene held", settled(1, hdr.String()))
	if !b.Has(entries["alps-clear"]) || b.Has(entries["sea"]) || b.Has(entries["alps-cloudy"]) || b.Has(entries["alps-old"]) {
		t.Fatal("b holds other items than the selected one")
	}
	// The catalog is held whole: b lists every item without a and serves the selected
	// item; an unselected item's content is not held.
	n.Partition(a, b)
	if l, err := b.Local.Ls(ctx, "scenes"); err != nil || len(l.Entries) != 4 {
		t.Fatalf("ls: %+v %v", l, err)
	}
	if _, err := b.Cat("scenes/alps-clear/visual.tif"); err != nil {
		t.Fatalf("cat selected: %v", err)
	}
	if _, err := b.Cat("scenes/sea/visual.tif"); err == nil {
		t.Fatal("unselected item readable without its holder")
	}
	n.Heal(a, b)

	// Restart: holdings persist and converge without refetching (a is partitioned).
	b.Stop()
	items = append(items[:3], nutest.Item{Path: "alps-new", BBox: alps, Cloud: 1, When: now})
	hdr2, entries2 := nutest.BuildCollection(t, a.Local.Store, items)
	publish(hdr2.String(), "2026-01-02T00:00:00Z")
	n.Partition(a, b)
	b.Start()
	b.Local.Atproto = atproto.New(f.Config())
	time.Sleep(3 * node.ClusterInterval)
	if !settled(1, hdr.String())() || !b.Has(entries["alps-clear"]) {
		fs, _ := b.Local.Follows(ctx)
		t.Fatalf("after restart: %+v", fs)
	}
	// The missed publication: a refresh re-resolves the name, the new catalog is fetched
	// from a and the new scene joins the held one.
	n.Heal(a, b)
	n.Peer(b, a)
	if _, err := b.Local.FollowRefresh(ctx, apiv0.FollowRefreshRequest{Dataset: "scenes"}); err != nil {
		t.Fatal(err)
	}
	nutest.Eventually(t, "the republished catalog and its new scene", settled(2, hdr2.String()))
	if !b.Has(entries2["alps-new"]) || !b.Has(entries2["alps-clear"]) || b.Has(entries2["sea"]) {
		t.Fatal("holdings after the republish")
	}
	fs, _ := b.Local.Follows(ctx)
	if fs[0].Selection.Items != 4 || fs[0].Selection.Selected != 2 {
		t.Fatalf("selection %+v", fs[0].Selection)
	}
}
