package collection

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

type rig struct {
	t      *testing.T
	st     *store.Store
	c      *cluster.Cluster
	m      *Manager
	now    time.Time
	wA, wB string
}

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func newRig(t *testing.T) *rig {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r := &rig{t: t, st: st, now: t0}
	key, _ := wire.GenerateNodeKey()
	r.c, err = cluster.Create(cluster.Options{Dir: filepath.Join(dir, "cluster"), Store: st, Now: func() time.Time { return r.now }}, "t", cluster.ModeLocal, key.Public().PeerID(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.wA, err = r.c.NewKey(); err != nil {
		t.Fatal(err)
	}
	if r.wB, err = r.c.NewKey(); err != nil {
		t.Fatal(err)
	}
	r.dataset(nil)
	r.m, err = Open(Config{DBPath: filepath.Join(dir, "collections.db"), Store: st, Cluster: r.c, Self: key.Public().PeerID(), Now: func() time.Time { return r.now }})
	if err != nil {
		t.Fatal(err)
	}
	r.m.ownKeys = func() map[string]bool { return nil } // a reader: holds no writer's items as their writer
	t.Cleanup(func() { r.m.Close() })
	return r
}

func (r *rig) dataset(expire *cluster.Expire) {
	facets := map[string]cluster.Facet{}
	for k, f := range stacFacets {
		facets[k] = cluster.Facet{Ptr: f.Ptr, Type: f.Type}
	}
	if _, err := r.c.Write(
		cluster.Op{Collection: cluster.NSKey, RKey: r.wA, Value: &cluster.Key{Role: cluster.RoleWriter}},
		cluster.Op{Collection: cluster.NSKey, RKey: r.wB, Value: &cluster.Key{Role: cluster.RoleWriter}},
		cluster.Op{Collection: cluster.NSDataset, RKey: "s2", Value: &cluster.Dataset{Source: cluster.Source{Local: true}, Writers: []string{r.wA, r.wB},
			Collection: &cluster.CollectionPolicy{Facets: facets, Expire: expire}}}); err != nil {
		r.t.Fatal(err)
	}
}

// scene stores an item (one file and a STAC-like metadata record) and returns its entry.
func (r *rig) scene(id string, sensed time.Time, bbox []any, cloud float64) tree.Entry {
	res, err := tree.BuildFS(fstest.MapFS{"visual.tif": {Data: []byte("pixels of " + id)}}, ".", tree.BuildOptions{Blobs: r.st, Nodes: r.st})
	if err != nil {
		r.t.Fatal(err)
	}
	mc, mb, err := drisl.Sum(map[string]any{"stac": map[string]any{"id": id, "bbox": bbox,
		"properties": map[string]any{"datetime": sensed.Format(time.RFC3339), "eo:cloud_cover": cloud}}})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.st.Put(mc, mb); err != nil {
		r.t.Fatal(err)
	}
	return tree.Entry{Kind: tree.KindDir, CID: res.Root, Size: res.Bytes, Meta: mc}
}

// head writes a writer's head holding leaves.
func (r *rig) head(did string, leaves map[string]tree.Entry) {
	root := buildCatalog(r.t, r.st, leaves)
	hc, hb, _ := drisl.Sum(tree.Header{V: 1, Root: root})
	if err := r.st.Put(hc, hb); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.c.WriteAs(did, cluster.Op{Collection: cluster.NSHead, RKey: "s2", Value: &cluster.Head{Target: hc.String(), Kind: "tree"}}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) reconcile() {
	r.t.Helper()
	r.c.Refresh()
	if err := r.m.Reconcile(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	// Run the fetch worker's job synchronously (content is local).
	rows, _ := r.m.db.Query(`SELECT path, entry FROM holdings WHERE ds = 's2' AND state = ?`, StateFetching)
	type job struct {
		p string
		e cid.CID
	}
	var jobs []job
	for rows.Next() {
		var j job
		var eb []byte
		_ = rows.Scan(&j.p, &eb)
		j.e, _ = cid.Decode(eb)
		jobs = append(jobs, j)
	}
	rows.Close()
	for _, j := range jobs {
		r.m.fetchItem("s2", j.p, j.e)
	}
}

func (r *rig) local() map[string]string {
	out, err := r.m.localStates("s2")
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *rig) rules(js string) *apiv0.CollectionRules {
	var rules []Rule
	if err := jsonUnmarshal(js, &rules); err != nil {
		r.t.Fatal(err)
	}
	res, err := r.m.SetRules("s2", rules, false)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

var alps, sea = []any{6.0, 46.0, 7.0, 47.0}, []any{-30.0, 30.0, -29.0, 31.0}

// TestSelection covers union heads with a conflict, the item stream, first-match regional
// and cloud rules with a rolling window, manual holds, budgets, the holdings log with
// overflow/resync/snapshot, the index following it, and writer expiry.
func TestSelection(t *testing.T) {
	r := newRig(t)
	clear := r.scene("clear", t0.Add(-time.Hour), alps, 10)
	x1, x2 := r.scene("x1", t0.Add(-time.Hour), alps, 0), r.scene("x2", t0.Add(-time.Hour), alps, 0)
	r.head(r.wA, map[string]tree.Entry{"2026/10/20/clear": clear, "2026/10/20/cloudy": r.scene("cloudy", t0.Add(-time.Hour), alps, 80), "2026/10/20/x": x1})
	r.head(r.wB, map[string]tree.Entry{"2026/10/20/clear": clear, "2026/10/18/old": r.scene("old", t0.Add(-60*time.Hour), alps, 5),
		"2026/10/20/sea": r.scene("sea", t0.Add(-time.Hour), sea, 0), "2026/10/20/x": x2})
	dry := r.rules(`[{"bbox": {"intersects": [5.9, 45.8, 10.5, 47.8]}, "cloud": {"max": 30}, "sensed": {"within": "48h"}}]`)
	_ = dry
	r.reconcile()

	// Union: five items, one conflict, the earlier writer's entry wins.
	page, err := r.m.Items("s2", apiv0.CollectionItemsQuery{Since: "0"})
	if err != nil || len(page.Items) != 5 {
		t.Fatalf("stream %+v %v", page, err)
	}
	cs, _ := r.m.Conflicts("s2")
	if len(cs) != 1 || cs[0].Path != "2026/10/20/x" || cs[0].Loser != r.wB || cs[0].Entry != x2.CID.String() {
		t.Fatalf("conflicts %+v", cs)
	}
	if got := r.local(); len(got) != 2 || got["2026/10/20/clear"] != StateComplete || got["2026/10/20/x"] != StateComplete {
		t.Fatalf("selected %v", got)
	}
	it, _ := r.m.Item("s2", "2026/10/20/x")
	if it.Entry != x1.CID.String() || it.Facets["cloud"] != 0.0 {
		t.Fatalf("item %+v", it)
	}
	// Continuing the stream from its cursor yields nothing new.
	if p2, _ := r.m.Items("s2", apiv0.CollectionItemsQuery{Since: page.NextCursor}); len(p2.Items) != 0 {
		t.Fatalf("stream repeated %d items", len(p2.Items))
	}

	// Rolling: 48 h later both leave the window and are released.
	r.now = t0.Add(48 * time.Hour)
	r.reconcile()
	if got := r.local(); len(got) != 0 {
		t.Fatalf("after the window: %v", got)
	}
	// Manual hold and release.
	if err := r.m.Hold("s2", "2026/10/20/sea"); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if got := r.local(); got["2026/10/20/sea"] != StateComplete {
		t.Fatalf("manual hold %v", got)
	}
	if err := r.m.Release("s2", "2026/10/20/sea"); err != nil || len(r.local()) != 0 {
		t.Fatalf("release: %v %v", err, r.local())
	}
	// Budget: room for one item; the others pause and status reports the shortfall.
	r.rules(fmt.Sprintf(`[{"cloud": {"max": 100}, "max_bytes": %d}]`, clear.Size))
	r.reconcile()
	st, err := r.m.Status("s2")
	if err != nil || st.Local[StateComplete] != 1 || st.Local[StatePaused] != 4 || st.Shortfall == 0 || st.Items != 5 || st.Conflicts != 1 ||
		st.FacetAbsent["cloud"] != 0 || st.FacetAbsent["platform"] != 5 {
		t.Fatalf("budget status %+v %v", st, err)
	}

	// Holdings log: a cursor older than the log answers resync; a snapshot pages; the
	// index follows the log to the same holdings.
	r.m.cfg.MaxLog = 2
	r.rules(`[]`)
	r.reconcile()
	lo, hi := r.m.LogMark("s2")
	if lo != hi-1 {
		t.Fatalf("log not trimmed: %d..%d", lo, hi)
	}
	if p, _ := r.m.Holdings("", cluster.HoldingsRequest{Dataset: "s2", Epoch: r.m.epoch, Since: 1}); !p.Resync {
		t.Fatal("old cursor did not resync")
	}
	if p, _ := r.m.Holdings("", cluster.HoldingsRequest{Dataset: "s2", Epoch: r.m.epoch, Since: hi}); p.Resync || len(p.Entries) != 0 {
		t.Fatalf("current cursor %+v", p)
	}
	r.rules(`[{"platform": {"in": []}, "hold": false}, {"cloud": {"max": 50}}]`) // four items
	r.reconcile()
	fetch := func(req cluster.HoldingsRequest) (*cluster.HoldingsPage, error) {
		req.Limit = 1
		return r.m.Holdings("", req)
	}
	if err := r.m.follow("s2", "peer-a", cursor{}, fetch); err != nil {
		t.Fatal(err)
	}
	counts, _, _ := r.m.copies("s2", map[string]bool{"peer-a": true}, nil)
	if len(counts) != 4 { // clear, x, sea, old (cloud <= 50)
		t.Fatalf("index counts %v, local %v", counts, r.local())
	}
	r.rules(`[]`)
	r.reconcile()
	if err := r.m.follow("s2", "peer-a", cursor{r.m.epoch, hi}, fetch); err != nil {
		t.Fatal(err)
	}
	if counts, _, _ := r.m.copies("s2", map[string]bool{"peer-a": true}, nil); len(counts) != 0 {
		t.Fatalf("index after releases %v", counts)
	}
}

// TestWriterExpire covers coalesced writer head writes through Put/Flush and expiry.
func TestWriterExpire(t *testing.T) {
	r := newRig(t)
	r.dataset(&cluster.Expire{Facet: "sensed", After: "2d"})
	r.m.ownKeys = nil
	fresh, old := r.scene("fresh", t0, alps, 0), r.scene("old", t0.Add(-24*time.Hour), alps, 0)
	if err := r.m.Put("s2", "a/fresh", &fresh); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Put("s2", "a/old", &old); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Flush(context.Background(), "s2"); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	st, _ := r.m.Status("s2")
	if st.Items != 2 || len(st.Writers) != 1 || st.Writers[0].DID != r.wA || st.Local[StateComplete] != 2 {
		t.Fatalf("status %+v", st)
	}
	r.now = t0.Add(36 * time.Hour) // "old" is 60 h old
	r.reconcile()                  // the writer prunes it; the merged view follows on the next pass
	r.reconcile()
	if st, _ = r.m.Status("s2"); st.Items != 1 {
		t.Fatalf("after expiry: %+v", st)
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// TestUnionVolume pushes overlapping items through two writers' heads in batches and
// checks the merged stream has every item exactly once; it logs catalog bytes per item and
// the re-evaluation cost (rough figures; benchmarks belong to hardening, 0068).
func TestUnionVolume(t *testing.T) {
	r := newRig(t)
	const n = 600
	leaves := []map[string]tree.Entry{{}, {}}
	for i := range n {
		e := r.scene(fmt.Sprint("s", i), t0.Add(-time.Duration(i)*time.Minute), alps, float64(i%100))
		p := fmt.Sprintf("2026/10/%02d/s%04d", 10+i/100, i)
		if i < n*2/3 {
			leaves[0][p] = e
		}
		if i >= n/3 {
			leaves[1][p] = e
		}
		if i%100 == 99 { // a head write per batch, as coalesced writers do
			r.head(r.wA, leaves[0])
			r.head(r.wB, leaves[1])
			r.reconcile()
		}
	}
	page, err := r.m.Items("s2", apiv0.CollectionItemsQuery{Since: "0", Limit: 1000})
	if err != nil || len(page.Items) != n {
		t.Fatalf("stream has %d items, want %d (%v)", len(page.Items), n, err)
	}
	seen := map[string]bool{}
	for _, it := range page.Items {
		if seen[it.Path] || it.Removed {
			t.Fatalf("duplicate or removed %s", it.Path)
		}
		seen[it.Path] = true
	}
	st, _ := r.m.Status("s2")
	ws, err := r.st.Walk(context.Background(), catalogPin(cid.MustParse(st.Root), ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	var bytes int
	_, _ = r.st.Walk(context.Background(), catalogPin(cid.MustParse(st.Root), ""), func(c cid.CID, _ bool) error {
		b, _ := r.st.Get(c)
		bytes += len(b)
		return nil
	})
	start := time.Now()
	r.rules(`[{"cloud": {"max": 50}, "sensed": {"within": "6h"}}]`)
	r.reconcile()
	t.Logf("%d items: catalog %d objects, %.0f B/item (metadata ~%d B); full re-evaluation and fetch of the selection %v",
		n, ws.Objects, float64(bytes)/n, len(mustGet(t, r, page.Items[0].Meta)), time.Since(start))
}

func mustGet(t *testing.T, r *rig, c string) []byte {
	b, err := r.st.Get(cid.MustParse(c))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
