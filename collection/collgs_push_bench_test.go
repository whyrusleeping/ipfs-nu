package collection_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/client"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/collection"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/nutest"
	"github.com/whyrusleeping/nu/store"
)

// producerFront serves a node's producer listener (push token auth) across node restarts;
// while the node is down it answers 503, as a stopped daemon's port refuses.
type producerFront struct {
	mu    sync.RWMutex
	nd    *nutest.Node
	local *node.Local
	h     http.Handler
}

func (p *producerFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.nd.Running() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if p.local != p.nd.Local {
		p.mu.RUnlock()
		p.mu.Lock()
		if p.local != p.nd.Local {
			p.local = p.nd.Local
			p.h = api.NewProducer(p.nd.Local, api.ProducerConfig{Hosts: []string{"127.0.0.1"}, Authorize: func(tok string) ([]string, bool) {
				return []string{"s2"}, tok == "nu_pt_bench"
			}}, nil)
		}
		p.mu.Unlock()
		p.mu.RLock()
	}
	p.h.ServeHTTP(w, r)
}

func (p *producerFront) restart() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nd.Restart()
}

// TestCollGSPush is the CollGS ingest run in compressed time: a producer pushes N items
// of the demo's shape (one file plus a ~1.4 KiB Matadisco/STAC metadata record) through
// the producer listener (push token) to two receiving members, as the pilot's torrentizer
// would, with 8 pushes in flight. Midway one receiving member restarts (the producer
// retries) and the producer "restarts", resending its last 1000 items. A third member
// runs the holdings index. It checks that both members wrote every item exactly once, that
// every member converges on one merged catalog of N items with no conflicts, that the item
// stream lists each item once, and that the index counts two copies of every item; it
// logs throughput, latencies and catalog/database sizes. Files are small: this measures the
// item, catalog and index path, not byte throughput. Opt-in: NU_BENCH_COLLGS_PUSH=20000.
func TestCollGSPush(t *testing.T) {
	v := os.Getenv("NU_BENCH_COLLGS_PUSH")
	if v == "" {
		t.Skip("opt-in: NU_BENCH_COLLGS_PUSH=20000 (items)")
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1000 {
		t.Fatalf("NU_BENCH_COLLGS_PUSH=%q: want an item count >= 1000", v)
	}
	// Deployment-like intervals: 1 s cluster heartbeats, the default 10 s head writes.
	for k, val := range map[*time.Duration]time.Duration{&node.ClusterInterval: time.Second, &node.HeadInterval: 10 * time.Second} {
		old := *k
		*k = val
		t.Cleanup(func() { *k = old })
	}
	node.CollectionIndex = true
	t.Cleanup(func() { node.CollectionIndex = false })

	nw := nutest.New(t, 3)
	a, b, c := nw.Nodes[0], nw.Nodes[1], nw.Nodes[2]
	if _, err := a.Local.ClusterInit(ctx(t), apiv0.ClusterInitRequest{Name: "collgs", Mode: cluster.ModePrivate}); err != nil {
		t.Fatal(err)
	}
	join(t, a, b)
	join(t, a, c)
	wa, _ := a.Local.NetCluster().NewKey()
	wb, _ := b.Local.NetCluster().NewKey()
	if _, err := a.Local.NetCluster().Write(
		cluster.Op{Collection: cluster.NSKey, RKey: wa, Value: &cluster.Key{Role: cluster.RoleWriter}},
		cluster.Op{Collection: cluster.NSKey, RKey: wb, Value: &cluster.Key{Role: cluster.RoleWriter}}); err != nil {
		t.Fatal(err)
	}
	facets := map[string]apiv0.FacetDecl{}
	for k, f := range collection.BenchFacets {
		facets[k] = apiv0.FacetDecl{Ptr: f.Ptr, Type: f.Type}
	}
	if _, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "s2", Collection: true, Writers: []string{wa, wb}, Facets: facets}); err != nil {
		t.Fatal(err)
	}
	nutest.Eventually(t, "writer keys and dataset on b", func() bool {
		_, ok := b.Local.NetCluster().View().Dataset("s2")
		return ok && b.Local.NetCluster().View().Keys[wb] != nil
	})

	fronts := map[*nutest.Node]*producerFront{}
	clients := map[*nutest.Node]*client.Client{}
	for _, nd := range []*nutest.Node{a, b} {
		fronts[nd] = &producerFront{nd: nd}
		srv := httptest.NewServer(fronts[nd])
		t.Cleanup(srv.Close)
		clients[nd] = client.New(srv.URL).WithToken("nu_pt_bench")
	}

	end := time.Now().UTC()
	type outcome struct{ id string }
	var mu sync.Mutex
	got := map[string]map[string]outcome{a.Name: {}, b.Name: {}} // node → path → push
	var retries, dups atomic.Int64
	var lat []time.Duration
	// push uploads item i's file and pushes it to nd, retrying (as a producer does) until
	// the member accepts it.
	push := func(nd *nutest.Node, i int) {
		id, path, meta := collection.BenchScene(i, end)
		mj, _ := json.Marshal(meta)
		data := []byte(fmt.Sprintf("%s true-colour COG stand-in %0512d", id, i))
		for attempt := 0; ; attempt++ {
			if attempt > 0 {
				retries.Add(1)
				time.Sleep(min(time.Duration(attempt)*100*time.Millisecond, 2*time.Second))
			}
			if attempt > 600 {
				t.Errorf("push %d to %s: gave up", i, nd.Name)
				return
			}
			cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			s := time.Now()
			up, err := clients[nd].CollectionUpload(cctx, "s2", bytes.NewReader(data), int64(len(data)))
			if err != nil {
				cancel()
				continue
			}
			p, err := clients[nd].CollectionPush(cctx, "s2", apiv0.CollectionPushRequest{Path: path, Meta: mj,
				Files: map[string]apiv0.CollectionPushFile{id + "_tci.tif": {Upload: up.CID}}}, 0)
			cancel()
			if err != nil {
				continue
			}
			mu.Lock()
			lat = append(lat, time.Since(s))
			if prev, ok := got[nd.Name][path]; ok && (prev.id != p.ID || !p.Duplicate) {
				// A resend of an accepted push must answer it, not a new push.
				t.Errorf("%s %s: resend answered %+v, first accept %s", nd.Name, path, p, prev.id)
			}
			if p.Duplicate {
				dups.Add(1)
			}
			got[nd.Name][path] = outcome{p.ID}
			mu.Unlock()
			return
		}
	}
	const inflight = 8
	run := func(lo, hi int) {
		work := make(chan int)
		var wg sync.WaitGroup
		for range inflight {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range work {
					push(a, i)
					push(b, i)
				}
			}()
		}
		for i := lo; i < hi; i++ {
			work <- i
		}
		close(work)
		wg.Wait()
	}
	start := time.Now()
	half := n / 2
	run(0, half)
	// The producer restarts and resends its last 1000 items; b restarts while the
	// producer continues.
	run(max(0, half-1000), half)
	restarted := make(chan struct{})
	go func() {
		defer close(restarted)
		time.Sleep(2 * time.Second)
		fronts[b].restart()
	}()
	run(half, n)
	<-restarted
	accepted := time.Since(start)
	slices.Sort(lat)
	t.Logf("pushed %d items to 2 members in %v: %.0f items/s accepted (%.1fM items/day); push latency (upload+push) p50 %v p99 %v; %d retries, %d duplicate answers",
		n, accepted.Round(time.Millisecond), float64(n)/accepted.Seconds(), float64(n)/accepted.Seconds()*86400/1e6,
		lat[len(lat)/2].Round(time.Microsecond), lat[len(lat)*99/100].Round(time.Microsecond), retries.Load(), dups.Load())

	// Every push written on both receiving members, each path once.
	for _, nd := range []*nutest.Node{a, b} {
		within(t, nd.Name+" wrote every push", 10*time.Minute, func() bool {
			// The push log lists each path at its last change, so a path can move past
			// the cursor while it is followed; the latest state per path counts.
			state := map[string]string{}
			cursor := "0"
			for {
				page, err := nd.Local.CollectionPushes(context.Background(), "s2", cursor, 1000)
				if err != nil {
					return false
				}
				for _, p := range page.Items {
					state[p.Path] = p.State
				}
				if len(page.Items) == 0 {
					break
				}
				cursor = page.NextCursor
			}
			written := 0
			for _, s := range state {
				if s == apiv0.PushWritten {
					written++
				}
			}
			return len(state) == n && written == n
		})
	}
	writtenAt := time.Since(start)
	within(t, "one merged catalog of every item on every member", 10*time.Minute, func() bool {
		root := status(a).Root
		for _, nd := range []*nutest.Node{a, b, c} {
			if st := status(nd); st.Root != root || st.Items != n || st.Conflicts != 0 || len(st.Writers) != 2 {
				return false
			}
		}
		return root != ""
	})
	merged := time.Since(start)
	t.Cleanup(func() {
		if t.Failed() {
			for _, nd := range []*nutest.Node{a, b, c} {
				st := status(nd)
				j, _ := json.Marshal(struct {
					Local map[string]int
					Index any
				}{st.Local, st.Index})
				t.Logf("%s: %s", nd.Name, j)
			}
		}
	})
	within(t, "the index on c counts two copies of every item", 10*time.Minute, func() bool {
		st := status(c)
		return st.Index != nil && st.Index.Copies["2"] == n
	})
	indexed := time.Since(start)
	t.Logf("all written on both members after %v (%.0f items/s); merged catalog on all 3 members after %v; index complete after %v",
		writtenAt.Round(time.Millisecond), float64(n)/writtenAt.Seconds(), merged.Round(time.Millisecond), indexed.Round(time.Millisecond))

	// The item stream on c lists every item once; status and the dashboard queries.
	seen := map[string]bool{}
	cursor := "0"
	for {
		page, err := c.Local.CollectionItems(ctx(t), "s2", apiv0.CollectionItemsQuery{Since: cursor, Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			if seen[it.Path] || it.Removed {
				t.Fatalf("stream: %s repeated or removed", it.Path)
			}
			seen[it.Path] = true
		}
		if len(page.Items) == 0 {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != n {
		t.Fatalf("stream lists %d items, want %d", len(seen), n)
	}
	for i := range n {
		if _, path, _ := collection.BenchScene(i, end); !seen[path] {
			t.Fatalf("item %d (%s) missing from the merged catalog", i, path)
		}
	}
	st := status(c)
	pin := store.Pin{CID: cid.MustParse(st.Root), Mode: store.Recursive, Profile: tree.CatalogProfile}
	var catBytes int
	if _, err := c.Local.Store.Walk(context.Background(), pin, func(x cid.CID, _ bool) error {
		b, _ := c.Local.Store.Get(x)
		catBytes += len(b)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dbSize := func(nd *nutest.Node, rel string) string {
		var total int64
		for _, suf := range []string{"", "-wal"} {
			if fi, err := os.Stat(filepath.Join(nd.Local.Repo.Root(), rel) + suf); err == nil {
				total += fi.Size()
			}
		}
		return fmt.Sprintf("%.1f MiB", float64(total)/(1<<20))
	}
	t.Logf("catalog on c: %d B = %.0f B/item; collections.db: a %s, c %s", catBytes, float64(catBytes)/float64(n),
		dbSize(a, "cluster/net/collections.db"), dbSize(c, "cluster/net/collections.db"))
	for _, q := range []struct {
		name string
		q    apiv0.CollectionItemsQuery
	}{
		{"overview (rarest, limit 1000)", apiv0.CollectionItemsQuery{Rarest: true, Limit: 1000}},
		{"member items (holder a, limit 1000)", apiv0.CollectionItemsQuery{Holder: a.ID.String(), Limit: 1000}},
	} {
		s := time.Now()
		if _, err := c.Local.CollectionItems(ctx(t), "s2", q.q); err != nil {
			t.Fatal(err)
		}
		t.Logf("index on c: %s %v", q.name, time.Since(s).Round(time.Microsecond))
	}
}

// within waits up to d for cond, polling twice a second.
func within(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
