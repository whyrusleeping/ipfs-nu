package collection

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
)

// publish writes the follow's head: a published collection header (root plus the
// publisher's facet declarations) as the admin's verified target.
func (r *rig) publish(name string, leaves map[string]tree.Entry, facets map[string]FacetSpec) cid.CID {
	root := buildCatalog(r.t, r.st, leaves)
	hc, hb, _ := drisl.Sum(tree.Header{V: 1, Root: root, Collection: map[string]any{"facets": facetsGeneric(facets)}})
	if err := r.st.Put(hc, hb); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.c.Write(cluster.Op{Collection: cluster.NSHead, RKey: name, Value: &cluster.Head{Target: hc.String(), Kind: "tree", Observed: &cluster.Observed{Rev: "3a", Commit: hc.String()}}}); err != nil {
		r.t.Fatal(err)
	}
	return hc
}

func (r *rig) reconcileAll() {
	r.t.Helper()
	r.c.Refresh()
	if err := r.m.Reconcile(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	rows, _ := r.m.db.Query(`SELECT ds, path, entry FROM holdings WHERE state = ?`, StateFetching)
	type job struct {
		ds, p string
		e     cid.CID
	}
	var jobs []job
	for rows.Next() {
		var j job
		var eb []byte
		_ = rows.Scan(&j.ds, &j.p, &eb)
		j.e, _ = cid.Decode(eb)
		jobs = append(jobs, j)
	}
	rows.Close()
	for _, j := range jobs {
		r.m.fetchItem(j.ds, j.p, j.e)
	}
}

// A followed collection (0058): the follow's head is its single writer, the facets come
// from the published header, rules set before the first sync are deferred and then
// apply, rescue rules are refused, the rolling window and a rule change re-evaluate
// without refetching held items (holdings stay complete, no new fetching transition),
// a newer publication converges by catalog diff, `max_size` pauses matches over the
// budget with a shortfall, and Forget releases every pin.
func TestFollowSelection(t *testing.T) {
	r := newRig(t)
	const ds = "fol"
	if _, err := r.c.Write(cluster.Op{Collection: cluster.NSDataset, RKey: ds, Value: &cluster.Dataset{
		Source:     cluster.Source{Name: "at://did:plc:pub/io.ipfs.nu.name/s2", Follow: &cluster.Follow{Select: true}},
		Collection: &cluster.CollectionPolicy{}}}); err != nil {
		t.Fatal(err)
	}
	// Rules before any sync: accepted unchecked; rescue is refused.
	rules := func(js string) *apiv0.CollectionRules {
		var rs []Rule
		if err := jsonUnmarshal(js, &rs); err != nil {
			t.Fatal(err)
		}
		res, err := r.m.SetRules(ds, rs, false)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	rules(`[{"bbox": {"intersects": [5.9, 45.8, 10.5, 47.8]}, "cloud": {"max": 30}, "sensed": {"within": "48h"}}]`)
	var rs []Rule
	_ = jsonUnmarshal(`[{"rescue": {"below": 2}}]`, &rs)
	if _, err := r.m.SetRules(ds, rs, false); !errors.Is(err, ErrRescueFollow) {
		var ae *apiv0.Error
		if !errors.As(err, &ae) || ae.Code != apiv0.CodeInvalidArgument {
			t.Fatalf("rescue on a follow: %v", err)
		}
	}
	if _, ok := r.m.SyncedHead(ds); ok {
		t.Fatal("synced before any head")
	}

	clear := r.scene("clear", t0.Add(-time.Hour), alps, 10)
	cloudy := r.scene("cloudy", t0.Add(-time.Hour), alps, 80)
	sea := r.scene("sea", t0.Add(-time.Hour), sea, 0)
	old := r.scene("old", t0.Add(-40*time.Hour), alps, 5)
	leaves := map[string]tree.Entry{"2026/10/20/clear": clear, "2026/10/20/cloudy": cloudy, "2026/10/20/sea": sea, "2026/10/18/old": old}
	head := r.publish(ds, leaves, stacFacets)
	r.reconcileAll()
	if h, ok := r.m.SyncedHead(ds); !ok || h != head {
		t.Fatalf("synced head %s %v", h, ok)
	}
	sum, err := r.m.FollowSummary(ds)
	if err != nil || !sum.CatalogSynced || sum.Items != 4 || sum.Selected != 2 || sum.Held != 2 || len(sum.Facets) != len(stacFacets) || sum.Rules != 1 {
		t.Fatalf("summary %+v %v", sum, err)
	}
	got := func() map[string]string {
		out, err := r.m.localStates(ds)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if g := got(); g["2026/10/20/clear"] != StateComplete || g["2026/10/18/old"] != StateComplete || len(g) != 2 {
		t.Fatalf("held %v", g)
	}
	_, seq0 := r.m.LogMark(ds)

	// A rule change that keeps `clear` and `old`: no new transition for them (no refetch),
	// `sea` joins; an unknown facet is refused now that the facets are known.
	rules(`[{"cloud": {"max": 20}}]`)
	r.reconcileAll()
	if g := got(); g["2026/10/20/clear"] != StateComplete || g["2026/10/20/sea"] != StateComplete || g["2026/10/18/old"] != StateComplete || len(g) != 3 {
		t.Fatalf("after rule change %v", g)
	}
	page, _ := r.m.Holdings("", cluster.HoldingsRequest{Dataset: ds, Epoch: r.m.epoch, Since: seq0})
	for _, e := range page.Entries {
		if e.Path == "2026/10/20/clear" || e.Path == "2026/10/18/old" {
			t.Fatalf("held item re-transitioned: %+v", e)
		}
	}
	_ = jsonUnmarshal(`[{"nope": {"eq": "x"}}]`, &rs)
	if _, err := r.m.SetRules(ds, rs, false); err == nil {
		t.Fatal("unknown facet accepted after sync")
	}

	// Rolling window: 48 h on, `old` and the rest leave the window.
	rules(`[{"sensed": {"within": "48h"}}]`)
	r.reconcileAll()
	if g := got(); len(g) != 4 {
		t.Fatalf("window %v", g)
	}
	r.now = t0.Add(10 * time.Hour)
	r.reconcileAll()
	if g := got(); len(g) != 3 || g["2026/10/18/old"] != "" {
		t.Fatalf("window moved %v", g)
	}

	// A newer publication replaces an item and adds one: only the diff is evaluated.
	clear2 := r.scene("clear2", t0.Add(-time.Hour), alps, 12)
	fresh := r.scene("fresh", t0.Add(time.Hour), alps, 1)
	leaves["2026/10/20/clear"] = clear2
	leaves["2026/10/21/fresh"] = fresh
	delete(leaves, "2026/10/20/cloudy")
	head2 := r.publish(ds, leaves, stacFacets)
	r.reconcileAll()
	if h, _ := r.m.SyncedHead(ds); h != head2 {
		t.Fatalf("not resynced: %s", h)
	}
	if g := got(); len(g) != 3 || g["2026/10/20/clear"] != StateComplete || g["2026/10/21/fresh"] != StateComplete || g["2026/10/20/cloudy"] != "" {
		t.Fatalf("after republish %v", g)
	}
	if it, _ := r.m.Item(ds, "2026/10/20/clear"); it.Entry != clear2.CID.String() {
		t.Fatalf("item not replaced: %+v", it)
	}

	// max_size over all holdings: the three held items stay (budgets never evict), the
	// new match pauses with a shortfall.
	if _, err := r.c.Write(cluster.Op{Collection: cluster.NSDataset, RKey: ds, Value: &cluster.Dataset{
		Source:     cluster.Source{Name: "at://did:plc:pub/io.ipfs.nu.name/s2", Follow: &cluster.Follow{Select: true, MaxSize: int64(clear2.Size)}},
		Collection: &cluster.CollectionPolicy{}}}); err != nil {
		t.Fatal(err)
	}
	rules(`[{"cloud": {"max": 100}}]`)
	r.reconcileAll()
	sum, _ = r.m.FollowSummary(ds)
	if sum.Held != 3 || sum.Paused != 1 || sum.Shortfall == 0 || sum.Selected != 4 {
		t.Fatalf("max_size %+v", sum)
	}

	// Forget releases the item and catalog pins.
	released, err := r.m.Forget(ds)
	if err != nil || len(released) < 2 {
		t.Fatalf("forget %v %v", released, err)
	}
	pins, _ := r.st.Pins()
	for _, p := range pins {
		if strings.HasPrefix(p.Name, "catalog/"+ds) || strings.HasPrefix(p.Name, ds+"/") {
			t.Fatalf("pin left: %+v", p)
		}
	}
	if _, ok := r.m.SyncedHead(ds); ok {
		t.Fatal("state left after forget")
	}
}
