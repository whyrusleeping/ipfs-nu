package pilot_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/bench/fixtures"
	"github.com/whyrusleeping/nu/bench/pilot"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/nutest"
	"github.com/whyrusleeping/nu/store"
)

const note = "Harness smoke results, not pilot evidence: synthetic bench/fixtures shapes on one shared machine, in-process nodes over loopback QUIC, compressed cluster timings. No support or scale claim follows from them."

type config struct {
	shapes   []fixtures.Shape
	p        fixtures.Params
	versions int
	cluster  bool
	out      string
	wait     time.Duration
}

func envConfig(t *testing.T) config {
	sel := os.Getenv("NU_BENCH_PILOT")
	if sel == "" {
		t.Skip("opt-in benchmark: NU_BENCH_PILOT=all (or shapes " + strings.Join(fixtures.Names(), ",") + "); see docs/pilots.md")
	}
	c := config{p: fixtures.Params{Seed: 1, Scale: 1}, versions: 4, cluster: os.Getenv("NU_BENCH_CLUSTER") != "0", out: os.Getenv("NU_BENCH_OUT"), wait: nutest.Scale(5 * time.Minute)}
	if sel == "all" {
		c.shapes = fixtures.Shapes
	} else {
		for _, name := range strings.Split(sel, ",") {
			s, ok := fixtures.Lookup(strings.TrimSpace(name))
			if !ok {
				t.Fatalf("unknown shape %q (have %v)", name, fixtures.Names())
			}
			c.shapes = append(c.shapes, s)
		}
	}
	if v := os.Getenv("NU_BENCH_SCALE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			t.Fatalf("NU_BENCH_SCALE=%q", v)
		}
		c.p.Scale = f
	}
	if v := os.Getenv("NU_BENCH_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("NU_BENCH_SEED=%q", v)
		}
		c.p.Seed = n
	}
	if v := os.Getenv("NU_BENCH_VERSIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 2 {
			t.Fatalf("NU_BENCH_VERSIONS=%q (at least 2)", v)
		}
		c.versions = n
	}
	return c
}

// Compressed cluster windows (production: docs/cluster.md). They keep production's
// ratios to the heartbeat roughly, as cluster/repair_net_test.go does.
const (
	heartbeat = 200 * time.Millisecond
	fresh     = 2 * time.Second
	downAfter = time.Second
	settle    = 500 * time.Millisecond
)

// TestPilot runs the 0034 methodology over the selected fixture shapes (opt-in; see the
// package documentation).
func TestPilot(t *testing.T) {
	cfg := envConfig(t)
	savedI, savedF, savedS := node.ClusterInterval, node.EvidenceFresh, node.ClusterDefaults
	t.Cleanup(func() { node.ClusterInterval, node.EvidenceFresh, node.ClusterDefaults = savedI, savedF, savedS })
	node.ClusterInterval, node.EvidenceFresh = heartbeat, fresh
	s := node.ClusterDefaults
	s.DownAfter, s.UpAfter, s.Settle, s.Grace, s.AuditInterval = downAfter, downAfter, settle, 0, 0
	node.ClusterDefaults = s

	rep := &pilot.Report{Note: note, Go: runtime.Version(), OS: runtime.GOOS + "/" + runtime.GOARCH, CPUModel: cpuModel(),
		CPUs: runtime.NumCPU(), MaxProcs: runtime.GOMAXPROCS(0), TmpDir: os.TempDir(), Seed: cfg.p.Seed, Scale: cfg.p.Scale, Versions: cfg.versions,
		Timings: pilot.Timings{Heartbeat: heartbeat.String(), EvidenceFresh: fresh.String(), DownAfter: downAfter.String(), UpAfter: downAfter.String(), Settle: settle.String()}}
	if out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
		rep.Commit = strings.TrimSpace(string(out))
		if st, err := exec.Command("git", "status", "--porcelain", "--untracked-files=no").Output(); err == nil && len(st) > 0 {
			rep.Commit += "+dirty"
		}
	}
	for _, shape := range cfg.shapes {
		res := &pilot.Shape{Name: shape.Name}
		rep.Shapes = append(rep.Shapes, res)
		// One more version than measured: the over-budget probe's update.
		vs := shape.Versions(cfg.p, cfg.versions+1)
		t.Run(shape.Name, func(t *testing.T) {
			t.Run("archive", func(t *testing.T) { runArchive(t, cfg, vs, res) })
			if shape.Audience != "institutional" {
				t.Run("share", func(t *testing.T) { runShare(t, cfg, vs, res) })
			}
			if cfg.cluster {
				t.Run("cluster", func(t *testing.T) { runCluster(t, cfg, vs, res) })
			}
		})
	}
	rep.MaxRSS = pilot.MaxRSS()
	md := rep.Markdown()
	t.Log("\n" + md)
	if cfg.out != "" {
		if err := os.MkdirAll(cfg.out, 0o755); err != nil {
			t.Fatal(err)
		}
		js, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(filepath.Join(cfg.out, "results.json"), append(js, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.out, "results.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func cpuModel() string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func fail(t *testing.T, res *pilot.Shape, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(t.Name()+": "+format, args...)
	res.Errors = append(res.Errors, msg)
	t.Error(msg)
}

func ctx(t *testing.T, d time.Duration) context.Context {
	c, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return c
}

// add adds dir unpinned (a dataset's pins hold what it references) and waits.
func add(t *testing.T, nd *nutest.Node, dir string, wait time.Duration) (apiv0.AddResult, error) {
	var res apiv0.AddResult
	c := ctx(t, wait)
	op, err := nd.Local.Add(c, apiv0.AddRequest{Path: dir, Recursive: true, NoAnnounce: true})
	if err != nil {
		return res, err
	}
	final, err := nd.Local.WaitOperation(c, op.ID, wait)
	if err != nil {
		return res, err
	}
	if final.State != apiv0.StateSucceeded {
		if final.Error != nil {
			return res, final.Error
		}
		return res, fmt.Errorf("add %s", final.State)
	}
	return res, json.Unmarshal(final.Result, &res)
}

func stats(t *testing.T, nd *nutest.Node) store.Stats {
	t.Helper()
	st, err := nd.Local.Store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func isCode(err error, code apiv0.Code) (*apiv0.Error, bool) {
	var ae *apiv0.Error
	return ae, errors.As(err, &ae) && ae.Code == code
}

// restore extracts dataset version tid on nd and verifies it against files.
func restore(t *testing.T, nd *nutest.Node, where string, v int, tid string, files []fixtures.File, served func() int64) pilot.Restore {
	out := filepath.Join(t.TempDir(), fmt.Sprintf("restore-%d", v))
	defer os.RemoveAll(out)
	s0 := served()
	start := time.Now()
	res, err := nd.TryGet("ds@"+tid, out)
	r := pilot.Restore{Where: where, V: v, Sec: time.Since(start).Seconds(), Bytes: res.Bytes, Served: served() - s0}
	if err != nil {
		t.Errorf("restore v%d on %s: %v", v, where, err)
		return r
	}
	if err := fixtures.Verify(out, files); err != nil {
		t.Errorf("restore v%d on %s: %v", v, where, err)
		return r
	}
	r.Verified = true
	return r
}

// runArchive: one node, an archive dataset (history keep_last 2 so the final prune has
// something to reclaim), every version imported from a source directory, exact-version
// restores, an over-budget update, then prune and GC.
func runArchive(t *testing.T, cfg config, vs [][]fixtures.File, res *pilot.Shape) {
	n := nutest.New(t, 1)
	a := n.Nodes[0]
	if _, err := a.Local.DatasetCreate(ctx(t, cfg.wait), node.ScopeLocal, apiv0.DatasetCreateRequest{Name: "ds", Archive: true, History: &apiv0.HistoryPolicy{KeepLast: 2}}); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "src")
	var prev []fixtures.File
	var tids []string
	for v := range cfg.versions {
		if err := fixtures.Materialize(src, prev, vs[v]); err != nil {
			t.Fatal(err)
		}
		ch := fixtures.Diff(prev, vs[v])
		s0, c0, start := stats(t, a), pilot.CPU(), time.Now()
		r, err := add(t, a, src, cfg.wait)
		if err != nil {
			t.Fatalf("v%d add: %v", v, err)
		}
		d, err := a.Local.DatasetUpdate(ctx(t, cfg.wait), node.ScopeLocal, "ds", apiv0.DatasetUpdateRequest{Target: r.CID})
		if err != nil {
			t.Fatalf("v%d update: %v", v, err)
		}
		el, cpu := time.Since(start), pilot.CPU()-c0
		s1 := stats(t, a)
		res.Archive = append(res.Archive, pilot.Version{V: v, Files: ch.Files, Logical: ch.Bytes, WholeFile: ch.WholeFileBytes(), Novel: ch.NovelBytes,
			NewBytes: d.Version.NewBytes, Stored: (s1.Blobs + s1.Outboards) - (s0.Blobs + s0.Outboards), DB: s1.DB - s0.DB,
			Physical: s1.Bytes - s0.Bytes, StoreTotal: s1.Bytes, AddSec: el.Seconds(), CPUSec: cpu.Seconds()})
		tids = append(tids, d.Version.TID)
		prev = vs[v]
	}
	none := func() int64 { return 0 }
	res.Restore = append(res.Restore, restore(t, a, "archive", 0, tids[0], vs[0], none))
	last := cfg.versions - 1
	res.Restore = append(res.Restore, restore(t, a, "archive", last, tids[last], vs[last], none))

	// Over budget: store.limit leaves half the next update's whole-file bytes.
	next := vs[cfg.versions]
	if err := fixtures.Materialize(src, prev, next); err != nil {
		t.Fatal(err)
	}
	need := fixtures.Diff(prev, next).WholeFileBytes()
	l0 := a.Local.Store.Limits()
	st := stats(t, a)
	l := l0
	l.Physical, l.FreeReserve, l.StateReserve = st.Bytes+need/2, 0, 0
	a.Local.Store.SetLimits(l)
	bu := &pilot.Budget{Limit: l.Physical, Usage: st.Bytes, Need: need}
	res.Budget = bu
	_, err := add(t, a, src, cfg.wait)
	if ae, ok := isCode(err, apiv0.CodeInsufficientStorage); ok {
		bu.Refused, bu.Code, bu.Message, bu.Action = true, string(ae.Code), ae.Message, ae.Action
	} else {
		fail(t, res, "over-budget add: %v", err)
	}
	if _, err := a.Local.Store.GC(ctx(t, cfg.wait), store.GCOptions{}); err != nil {
		t.Fatal(err)
	}
	vers, err := a.Local.DatasetVersions(ctx(t, cfg.wait), node.ScopeLocal, "ds")
	if err != nil {
		t.Fatal(err)
	}
	bu.HeldIntact = len(vers.Versions) == cfg.versions
	for _, vi := range vers.Versions {
		bu.HeldIntact = bu.HeldIntact && vi.Availability == apiv0.AvailComplete
	}
	bu.HeldIntact = bu.HeldIntact && restore(t, a, "archive", 0, tids[0], vs[0], none).Verified
	if !bu.HeldIntact {
		fail(t, res, "versions after the refused update: %+v", vers.Versions)
	}
	a.Local.Store.SetLimits(l0)
	bu.Interventions = []string{"raise store.limit"}
	if r, err := add(t, a, src, cfg.wait); err != nil {
		fail(t, res, "add after raising the limit: %v", err)
	} else if _, err := a.Local.DatasetUpdate(ctx(t, cfg.wait), node.ScopeLocal, "ds", apiv0.DatasetUpdateRequest{Target: r.CID}); err != nil {
		fail(t, res, "update after raising the limit: %v", err)
	} else {
		bu.AfterRaise = true
	}

	// Retention reclaim: keep_last 2 leaves every version but the last two prunable.
	dry, err := a.Local.DatasetPrune(ctx(t, cfg.wait), node.ScopeLocal, "ds", apiv0.PruneRequest{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	before := stats(t, a).Bytes
	pr, err := a.Local.DatasetPrune(ctx(t, cfg.wait), node.ScopeLocal, "ds", apiv0.PruneRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Local.Store.GC(ctx(t, cfg.wait), store.GCOptions{}); err != nil {
		t.Fatal(err)
	}
	res.Prune = &pilot.PruneStep{Pruned: len(pr.Pruned), FreeBytes: dry.FreeBytes, Reclaimed: before - stats(t, a).Bytes}
}

// runShare is the first-magic journey: a sharer daemon adds the tree and shares a link;
// a recipient with no daemon and no init gets it; a browser daemon gets it and serves a
// page on the <cid>.localhost origin; later versions are fetched by the same recipient.
func runShare(t *testing.T, cfg config, vs [][]fixtures.File, res *pilot.Shape) {
	n := nutest.New(t, 2, nutest.Options{API: true})
	sharer, browser := n.Nodes[0], n.Nodes[1]
	recipient := n.NewNode(false)
	sh := &pilot.Share{Steps: []string{"sharer: nu add -r <dir> (prints the link)", "recipient: nu get <link> -o <dir> (no daemon, no init)", "browser: open the gateway page"}}
	res.Share = sh
	src := filepath.Join(t.TempDir(), "src")
	var prev []fixtures.File
	for v := range cfg.versions {
		if err := fixtures.Materialize(src, prev, vs[v]); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		root := sharer.Add(src, true)
		addSec := time.Since(start).Seconds()
		link, err := sharer.Local.Share(ctx(t, cfg.wait), apiv0.ShareRequest{Target: root.String()})
		if err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), fmt.Sprintf("get-%d", v))
		s0 := sharer.Served.Load()
		start = time.Now()
		g := recipient.Get(link.Link, out)
		getSec, served := time.Since(start).Seconds(), sharer.Served.Load()-s0
		if err := fixtures.Verify(out, vs[v]); err != nil {
			fail(t, res, "v%d get: %v", v, err)
		}
		os.RemoveAll(out)
		if v > 0 {
			sh.Updates = append(sh.Updates, pilot.ShareUpdate{V: v, Novel: fixtures.Diff(prev, vs[v]).NovelBytes, Served: served, GetSec: getSec})
			prev = vs[v]
			continue
		}
		sh.AddSec, sh.GetSec, sh.Bytes, sh.Served = addSec, getSec, g.Bytes, served
		bout := filepath.Join(t.TempDir(), "browser")
		browser.Get(link.Link, bout)
		os.RemoveAll(bout)
		gw := browser.Daemon.GatewayAddr()
		_, port, _ := strings.Cut(gw, ":")
		req, _ := http.NewRequest(http.MethodGet, "http://"+gw+"/index.html", nil)
		req.Host = root.String() + ".localhost:" + port
		start = time.Now()
		hr, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, hr.Body)
		hr.Body.Close()
		sh.BrowseSec = time.Since(start).Seconds()
		if hr.StatusCode != http.StatusOK {
			fail(t, res, "gateway page: %s", hr.Status)
		}
		prev = vs[v]
	}
}

func storageCluster(t *testing.T, wait time.Duration, nodes ...*nutest.Node) {
	a := nodes[0]
	if _, err := a.Local.ClusterInit(ctx(t, wait), apiv0.ClusterInitRequest{Name: "pilot"}); err != nil {
		t.Fatal(err)
	}
	for _, nd := range nodes[1:] {
		inv, err := a.Local.ClusterInvite(ctx(t, wait), apiv0.ClusterInviteRequest{Peer: nd.ID.String()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := nd.Local.ClusterJoin(ctx(t, wait), apiv0.ClusterJoinRequest{Token: inv.Token}); err != nil {
			t.Fatal(err)
		}
	}
	if !until(wait, func() bool {
		for _, nd := range nodes {
			if len(nd.Local.NetCluster().View().MemberIDs()) != len(nodes) {
				return false
			}
		}
		return true
	}) {
		t.Fatal("membership did not converge")
	}
}

func until(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// runCluster: three storage members, an archive dataset with two copies written by the
// first member, the writer's surplus released, an old version restored on a member that
// holds only its own share, then one member lost and its copies restored by survivors.
func runCluster(t *testing.T, cfg config, vs [][]fixtures.File, res *pilot.Shape) {
	n := nutest.New(t, 3)
	a, b, c := n.Nodes[0], n.Nodes[1], n.Nodes[2]
	members := []*nutest.Node{a, b, c}
	storageCluster(t, cfg.wait, members...)
	if _, err := a.Local.DatasetCreate(ctx(t, cfg.wait), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "ds", Archive: true, Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	cl := &pilot.Cluster{Members: 3, Replicas: 2}
	res.Cluster = cl
	served := func(nds ...*nutest.Node) int64 {
		var s int64
		for _, nd := range nds {
			s += nd.Served.Load()
		}
		return s
	}
	physical := func(nds ...*nutest.Node) int64 {
		var s int64
		for _, nd := range nds {
			s += stats(t, nd).Bytes
		}
		return s
	}
	var holders []string
	for _, nd := range members {
		holders = append(holders, nd.ID.String())
	}
	timeout := cfg.wait.String()
	src := filepath.Join(t.TempDir(), "src")
	var prev []fixtures.File
	var tids []string
	for v := range cfg.versions {
		if err := fixtures.Materialize(src, prev, vs[v]); err != nil {
			t.Fatal(err)
		}
		ch := fixtures.Diff(prev, vs[v])
		tr0, st0, c0, start := served(members...), physical(members...), pilot.CPU(), time.Now()
		r, err := add(t, a, src, cfg.wait)
		if err != nil {
			t.Fatalf("v%d add: %v", v, err)
		}
		d, err := a.Local.DatasetUpdate(ctx(t, cfg.wait), node.ScopeCluster, "ds", apiv0.DatasetUpdateRequest{Target: r.CID})
		if err != nil {
			t.Fatalf("v%d update: %v", v, err)
		}
		addSec := time.Since(start).Seconds()
		row := pilot.ClusterVersion{V: v, Novel: ch.NovelBytes, WholeFile: ch.WholeFileBytes(), AddSec: addSec}
		if w, err := a.Local.DatasetWait(ctx(t, cfg.wait+time.Minute), node.ScopeCluster, "ds", apiv0.DatasetWaitRequest{Target: r.CID, Condition: apiv0.WaitReady, Timeout: timeout}); err != nil || !w.Satisfied {
			fail(t, res, "v%d ready: %v", v, err)
		}
		row.ReadySec = time.Since(start).Seconds()
		row.ReadyBeats = row.ReadySec / heartbeat.Seconds()
		if w, err := a.Local.DatasetWait(ctx(t, cfg.wait+time.Minute), node.ScopeCluster, "ds", apiv0.DatasetWaitRequest{Target: r.CID, Condition: apiv0.WaitHeld, Holders: holders, Mode: "owned", Timeout: timeout}); err != nil || !w.Satisfied {
			fail(t, res, "v%d held by every owner: %v", v, err)
		}
		row.HeldSec = time.Since(start).Seconds()
		row.CPUSec = (pilot.CPU() - c0).Seconds()
		row.Transfer, row.StoredTotal = served(members...)-tr0, physical(members...)-st0
		cl.Versions = append(cl.Versions, row)
		tids = append(tids, d.Version.TID)
		prev = vs[v]
	}

	// The writer keeps only what it owns: an explicit gc releases its surplus once every
	// owner confirms (S2).
	var dry *apiv0.ReleaseReport
	ok := until(cfg.wait, func() bool {
		r, err := a.Local.DatasetRelease(ctx(t, cfg.wait), node.ScopeCluster, "ds", apiv0.ReleaseRequest{DryRun: true})
		if err != nil {
			return false
		}
		dry = r
		ready := len(r.Deferred) == 0
		for _, cand := range r.Candidates {
			ready = ready && (cand.Kind != "surplus" || cand.OK)
		}
		return ready
	})
	if !ok {
		fail(t, res, "writer surplus never releasable: %+v", dry)
	} else {
		start := time.Now()
		rel, err := a.Local.DatasetRelease(ctx(t, cfg.wait), node.ScopeCluster, "ds", apiv0.ReleaseRequest{})
		if err != nil {
			fail(t, res, "release: %v", err)
		} else {
			cl.Release = &pilot.Release{Blobs: rel.ReleasedBlobs, Bytes: rel.ReleasedBytes, Sec: time.Since(start).Seconds()}
			cl.Interventions = append(cl.Interventions, "writer `dataset gc` (surplus release)")
		}
	}
	if _, err := a.Local.Store.GC(ctx(t, cfg.wait), store.GCOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, nd := range members {
		cl.Storage = append(cl.Storage, stats(t, nd).Bytes)
	}

	// Exact old version on a member holding only its share: the rest comes from peers.
	cl.Restore = append(cl.Restore, restore(t, b, "member", 0, tids[0], vs[0], func() int64 { return served(a, c) }))
	// The restore cached blobs on b; drop them so b holds its own share again.
	if _, err := b.Local.Store.GC(ctx(t, cfg.wait), store.GCOptions{}); err != nil {
		t.Fatal(err)
	}

	// Lose c. The founder (a) is the online repair authority (cluster.repair default).
	rp := &pilot.Repair{}
	cl.Repair = rp
	tr0, st0 := served(a, b), physical(a, b)
	start := time.Now()
	c.Kill()
	if !until(cfg.wait, func() bool {
		m := b.Local.NetCluster().View().Members[c.ID]
		return m.Value != nil && m.Value.State == cluster.StateUnavailable
	}) {
		fail(t, res, "lost member never marked unavailable")
		return
	}
	rp.DetectSec = time.Since(start).Seconds()
	start = time.Now()
	rp.Ready = until(cfg.wait, func() bool {
		d, err := b.Local.Dataset(ctx(t, cfg.wait), node.ScopeCluster, "ds")
		if err != nil {
			return false
		}
		ok := func(cs *apiv0.ContentState) bool {
			return cs.State == cluster.ReadyReady && cs.Copies != nil && cs.Copies.Min >= 2 &&
				!slices.ContainsFunc(cs.Confirmed, func(x apiv0.Confirmation) bool { return x.Peer == c.ID.Base36() })
		}
		return ok(&d.Content) && (d.Archive == nil || ok(d.Archive))
	})
	rp.RestoreSec = time.Since(start).Seconds()
	rp.Transfer, rp.Stored = served(a, b)-tr0, physical(a, b)-st0
	if !rp.Ready {
		d, _ := b.Local.Dataset(ctx(t, cfg.wait), node.ScopeCluster, "ds")
		fail(t, res, "copies not restored after losing a member: %+v", d)
	}
}
