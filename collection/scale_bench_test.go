package collection

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// CollGS-shaped workload helpers, shared with the push bench (package collection_test).

// BenchFacets are the facets the CollGS pilot declares over its metadata records.
var BenchFacets = map[string]FacetSpec{
	"sensed":   {Ptr: "/stac/properties/datetime", Type: TypeTime},
	"bbox":     {Ptr: "/stac/bbox", Type: TypeBBox},
	"cloud":    {Ptr: "/stac/properties/eo:cloud_cover", Type: TypeNumber},
	"platform": {Ptr: "/stac/properties/platform", Type: TypeString},
}

// BenchScene returns scene i of a CollGS-shaped feed: its STAC id, its item path
// (`YYYY/MM/DD/<id>`) and a deterministic metadata record shaped like the prototype's
// torrent metadata (a Matadisco record plus a STAC subset with a footprint polygon,
// about 1.4 KiB as DRISL; no packaging timestamp, so two receiving members produce the
// same entry). Scenes are spread over the days before end at about 13k a day.
func BenchScene(i int, end time.Time) (id, path string, meta map[string]any) {
	rng := rand.New(rand.NewSource(int64(i) + 1))
	sensed := end.Add(-time.Duration(i) * (24 * time.Hour / 13000)).Add(-time.Duration(rng.Intn(3600)) * time.Second).UTC()
	platform := []string{"sentinel-2a", "sentinel-2b", "sentinel-2c"}[rng.Intn(3)]
	// Tiles are unique per scene (for i < 1.6M), so ids and paths never collide.
	k := i / 2
	tile := fmt.Sprintf("%02d%c%c%c", 1+k%60, 'C'+rune(k/60%20), 'A'+rune(k/1200%26), 'A'+rune(k/31200%26))
	id = fmt.Sprintf("S2%c_%s_%s_%d_L2A", strings.ToUpper(platform[len(platform)-1:])[0], tile, sensed.Format("20060102"), i%2)
	path = sensed.Format("2006/01/02") + "/" + id
	w, s := -180+rng.Float64()*358, -80+rng.Float64()*150
	e, n := w+0.9+rng.Float64()*0.6, s+0.9+rng.Float64()*0.2
	ring := []any{}
	for _, p := range [][2]float64{{w, n}, {w + 0.02, s + 0.1}, {e - 0.1, s}, {e, s + 0.01}, {e - 0.02, n}, {w, n}} {
		ring = append(ring, []any{p[0] + rng.Float64()*1e-9, p[1] + rng.Float64()*1e-9})
	}
	created := sensed.Add(5 * time.Hour).Format("2006-01-02T15:04:05.000Z")
	resource := "https://earth-search.aws.element84.com/v1/collections/sentinel-2-l2a/items/" + id
	meta = map[string]any{
		"matadisco": map[string]any{
			"uri":         fmt.Sprintf("at://did:plc:otu6ccrwycke33kl3h4yjhwn/cx.vmx.matadisco/3my%010d", i),
			"publishedAt": created,
			"resource":    resource,
			"preview":     map[string]any{"mimeType": "image/jpeg", "url": resource + "/thumbnail"},
		},
		"stac": map[string]any{
			"id": id, "collection": "sentinel-2-l2a",
			"bbox":     []any{w, s, e, n},
			"geometry": map[string]any{"type": "Polygon", "coordinates": []any{ring}},
			"properties": map[string]any{
				"created": created, "datetime": sensed.Format("2006-01-02T15:04:05.000000Z"),
				"eo:cloud_cover": float64(rng.Intn(100_000_000)) / 1e6, "platform": platform,
				"instruments": []any{"msi"}, "proj:epsg": int64(32600 + rng.Intn(60)),
			},
		},
	}
	return id, path, meta
}

// benchWriter is a writer head built incrementally, as a receiving member's daemon does.
type benchWriter struct {
	did  string
	root cid.CID
	size uint64
}

type benchItem struct {
	path  string
	entry tree.Entry
}

func (r *rig) writeHead(w *benchWriter, items []benchItem) {
	ed, err := newEditor(r.st, w.root, w.size)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, it := range items {
		if err := ed.put(split(it.path), it.entry); err != nil {
			r.t.Fatal(err)
		}
	}
	if w.root, err = ed.flush(); err != nil {
		r.t.Fatal(err)
	}
	w.size = ed.root.size
	hc, hb, _ := drisl.Sum(tree.Header{V: 1, Root: w.root, Bytes: w.size})
	if err := r.st.Put(hc, hb); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.c.WriteAs(w.did, cluster.Op{Collection: cluster.NSHead, RKey: "s2", Value: &cluster.Head{Target: hc.String(), Kind: "tree", Size: int64(w.size)}}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) benchItem(i int, end time.Time) benchItem {
	id, path, meta := BenchScene(i, end)
	res, err := tree.BuildFS(fstest.MapFS{id + "_tci.tif": {Data: []byte("cog " + id)}}, ".", tree.BuildOptions{Blobs: r.st, Nodes: r.st})
	if err != nil {
		r.t.Fatal(err)
	}
	mc, mb, err := drisl.Sum(meta)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.st.Put(mc, mb); err != nil {
		r.t.Fatal(err)
	}
	return benchItem{path, tree.Entry{Kind: tree.KindDir, CID: res.Root, Size: 15 << 20, Meta: mc}}
}

// timed runs fn reps times and returns the median duration and the mean bytes allocated.
func timed(reps int, fn func()) (time.Duration, uint64) {
	var ds []time.Duration
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	before := ms.TotalAlloc
	for range reps {
		s := time.Now()
		fn()
		ds = append(ds, time.Since(s))
	}
	runtime.ReadMemStats(&ms)
	slices.Sort(ds)
	return ds[len(ds)/2], (ms.TotalAlloc - before) / uint64(reps)
}

func benchSizes(t *testing.T, env string) []int {
	v := os.Getenv(env)
	if v == "" {
		t.Skip("opt-in: " + env + "=20000,200000 (item counts)")
	}
	var out []int
	for _, f := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 100 {
			t.Fatalf("%s: bad item count %q", env, f)
		}
		out = append(out, n)
	}
	return out
}

// TestCollGSScale measures, at CollGS catalog sizes, the cost of one small writer head
// change on a member (catalog sync, union merge and selection), catalog bytes per item,
// the holdings-index queries a dashboard makes (rarest-first overview, a member's items,
// one item's holders, status with the copy histogram) with eight members' holdings, and
// selection re-evaluation (dry run, rule change, rolling-window edge). Opt-in:
// NU_BENCH_COLLGS_SCALE=20000,200000; results go to docs/validation.md.
func TestCollGSScale(t *testing.T) {
	for _, n := range benchSizes(t, "NU_BENCH_COLLGS_SCALE") {
		t.Run(strconv.Itoa(n), func(t *testing.T) { scaleRun(t, n) })
	}
}

func scaleRun(t *testing.T, n int) {
	r := newRig(t)
	// The CollGS facets replace the rig's.
	facets := map[string]cluster.Facet{}
	for k, f := range BenchFacets {
		facets[k] = cluster.Facet{Ptr: f.Ptr, Type: f.Type}
	}
	if _, err := r.c.Write(cluster.Op{Collection: cluster.NSDataset, RKey: "s2", Value: &cluster.Dataset{Source: cluster.Source{Local: true},
		Writers: []string{r.wA, r.wB}, Collection: &cluster.CollectionPolicy{Facets: facets}}}); err != nil {
		t.Fatal(err)
	}
	end := t0
	ws := []*benchWriter{{did: r.wA}, {did: r.wB}}
	sync := func() time.Duration {
		r.c.Refresh()
		s := time.Now()
		if err := r.m.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		return time.Since(s)
	}
	// Bulk load in batches of 2000 (both writers receive every item, as a producer
	// pushing each item to two receiving members does).
	build := time.Now()
	const batch = 2000
	var syncTotal time.Duration
	for lo := 0; lo < n; lo += batch {
		var items []benchItem
		for i := lo; i < min(lo+batch, n); i++ {
			items = append(items, r.benchItem(i, end))
		}
		for _, w := range ws {
			r.writeHead(w, items)
		}
		syncTotal += sync()
	}
	t.Logf("n=%d: built in %v (member sync of %d-item batches: %v total)", n, time.Since(build).Round(time.Millisecond), batch, syncTotal.Round(time.Millisecond))

	// Steady state: a head change of 3 new items from each writer (20k items/day with
	// 10 s head writes), as one member applies it.
	next := n
	var steady []time.Duration
	for range 5 {
		var items []benchItem
		for range 3 {
			items = append(items, r.benchItem(next, end.Add(time.Hour)))
			next++
		}
		for _, w := range ws {
			r.writeHead(w, items)
		}
		steady = append(steady, sync())
	}
	slices.Sort(steady)
	row, _ := r.m.loadColl("s2")
	pin := catalogPin(row.header, "")
	walk, _ := timed(3, func() { r.m.heldCatalog(context.Background(), pin) })
	var catBytes, objs int
	if _, err := r.st.Walk(context.Background(), pin, func(c cid.CID, _ bool) error {
		b, _ := r.st.Get(c)
		catBytes += len(b)
		objs++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	items := next
	t.Logf("n=%d: head change of 3 items/writer: member sync median %v (max %v); full catalog walk %v", items,
		steady[len(steady)/2].Round(time.Microsecond), steady[len(steady)-1].Round(time.Microsecond), walk.Round(time.Millisecond))
	t.Logf("n=%d: catalog %d objects, %d B = %.0f B/item; collections.db %s", items, objs, catBytes, float64(catBytes)/float64(items),
		fileSizes(r.m.cfg.DBPath))

	// Holdings index: eight members shaped like the prototype's nodes.
	r.now = end.Add(time.Hour)
	peers := make([]string, 8)
	fresh := map[string]bool{r.m.cfg.Self.String(): true}
	for i := range peers {
		k, _ := wire.GenerateNodeKey()
		peers[i] = k.Public().PeerID().String()
		fresh[peers[i]] = true
	}
	r.m.freshFn = func() map[string]bool { return fresh }
	europe := [4]float64{-10, 35, 30, 70}
	rows, err := r.m.db.Query(`SELECT ` + itemCols + ` FROM items WHERE ds = 's2' AND deleted = 0`)
	if err != nil {
		t.Fatal(err)
	}
	var all []*itemRow
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, it)
	}
	rows.Close()
	tx, _ := r.m.db.Begin()
	idxRows := 0
	rng := rand.New(rand.NewSource(1))
	for _, it := range all {
		sensed := it.facets["sensed"].Time
		bb := it.facets["bbox"].BBox
		hold := []bool{
			r.now.UnixMilli()-sensed < (7 * 24 * time.Hour).Milliseconds(), // rolling-7d
			r.now.UnixMilli()-sensed < (6 * time.Hour).Milliseconds(),      // rolling-6h
			Intersects(bb, europe),                                  // europe
			it.facets["cloud"].Num <= 5,                             // maxcloud-5
			rng.Intn(50) == 0, rng.Intn(50) == 0, rng.Intn(50) == 0, // rescuers
			false,
		}
		for p, h := range hold {
			if h {
				if err := applyIndex(tx, "s2", peers[p], cluster.HoldingsEntry{Path: it.path, Entry: it.entry, State: StateComplete, Src: SrcSelect, Bytes: int64(it.size)}); err != nil {
					t.Fatal(err)
				}
				idxRows++
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r.c.Refresh()
	r.m.cfg.Index = true
	q := func(name string, fn func() error) {
		d, alloc := timed(3, func() {
			if err := fn(); err != nil {
				t.Fatal(name, err)
			}
		})
		t.Logf("n=%d: %-40s median %8v, %6.1f MiB allocated", items, name, d.Round(time.Microsecond), float64(alloc)/(1<<20))
	}
	t.Logf("n=%d: index %d rows over 8 members", items, idxRows)
	q("overview (rarest first, limit 1000)", func() error {
		p, err := r.m.Items("s2", apiv0.CollectionItemsQuery{Rarest: true, Limit: apiv0.MaxPageLimit})
		if err == nil && (len(p.Items) != apiv0.MaxPageLimit || !slices.IsSortedFunc(p.Items, func(a, b apiv0.CollectionItem) int { return *a.Copies - *b.Copies })) {
			err = fmt.Errorf("overview: %d items, not rarest first", len(p.Items))
		}
		return err
	})
	q("member items (holder, limit 1000)", func() error {
		_, err := r.m.Items("s2", apiv0.CollectionItemsQuery{Holder: peers[2], Limit: 1000})
		return err
	})
	q("item with holders", func() error {
		_, err := r.m.Item("s2", all[len(all)/2].path)
		return err
	})
	q("status (index histogram, members)", func() error {
		_, err := r.m.Status("s2")
		return err
	})
	q("item stream page (since=0, limit 1000)", func() error {
		_, err := r.m.Items("s2", apiv0.CollectionItemsQuery{Since: "0", Limit: 1000})
		return err
	})
	q("facet query (europe, cloud<=20, limit 1000)", func() error {
		_, err := r.m.Items("s2", apiv0.CollectionItemsQuery{Match: []byte(`{"bbox": {"intersects": [-10, 35, 30, 70]}, "cloud": {"max": 20}}`), Limit: 1000})
		return err
	})

	// Selection re-evaluation: a regional/cloud rule with a rolling window, as the
	// prototype's data-managers run.
	rules := []Rule{}
	if err := jsonUnmarshal(`[{"bbox": {"intersects": [-10, 35, 30, 70]}, "cloud": {"max": 30}, "sensed": {"within": "48h"}}]`, &rules); err != nil {
		t.Fatal(err)
	}
	q("rules dry run", func() error {
		_, err := r.m.SetRules("s2", rules, true)
		return err
	})
	if _, err := r.m.SetRules("s2", rules, false); err != nil {
		t.Fatal(err)
	}
	full := sync()
	var local int
	_ = r.m.db.QueryRow(`SELECT COUNT(*) FROM holdings WHERE ds = 's2'`).Scan(&local)
	r.now = r.now.Add(10 * time.Minute)
	edge := sync()
	r.now = r.now.Add(10 * time.Minute)
	idle := sync()
	t.Logf("n=%d: rule change: full re-evaluation pass %v (%d items selected); 10 min later the window-edge pass %v, then %v",
		items, full.Round(time.Millisecond), local, edge.Round(time.Millisecond), idle.Round(time.Millisecond))
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	t.Logf("n=%d: Go heap in use after GC %.0f MiB; process max RSS so far %.0f MiB", items, float64(ms.HeapInuse)/(1<<20), float64(ru.Maxrss)/1024)
}

func fileSizes(db string) string {
	var total int64
	for _, suf := range []string{"", "-wal"} {
		if fi, err := os.Stat(db + suf); err == nil {
			total += fi.Size()
		}
	}
	return fmt.Sprintf("%.1f MiB (%s)", float64(total)/(1<<20), filepath.Base(db))
}
