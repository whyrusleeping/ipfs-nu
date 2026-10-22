package cluster_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/nutest"
	"github.com/whyrusleeping/nu/store"
)

// tune sets short repair/release/evidence settings for the nodes a test starts, and
// restores the defaults afterwards.
func tune(t *testing.T, fn func(s *node.ClusterSettings)) {
	t.Helper()
	saved, fresh := node.ClusterDefaults, node.EvidenceFresh
	t.Cleanup(func() { node.ClusterDefaults, node.EvidenceFresh = saved, fresh })
	node.EvidenceFresh = 2 * time.Second
	s := node.ClusterDefaults
	s.Repair, s.DownAfter, s.UpAfter = false, time.Second, time.Second
	s.Settle, s.Grace, s.AuditInterval = 300*time.Millisecond, 0, 0
	fn(&s)
	node.ClusterDefaults = s
}

func content(t *testing.T, nd *nutest.Node, ds string) apiv0.ContentState {
	t.Helper()
	d, err := nd.Local.Dataset(ctx(t), node.ScopeCluster, ds)
	if err != nil {
		t.Fatal(err)
	}
	return d.Content
}

func hasAlert(as []apiv0.Alert, code string) bool {
	return slices.ContainsFunc(as, func(a apiv0.Alert) bool { return a.Code == code })
}

// unpinExplicit drops nd's explicit pins of root (the writer's add pin), so only cluster
// placement keeps its content.
func unpinExplicit(t *testing.T, nd *nutest.Node, root cid.CID) {
	t.Helper()
	pins, err := nd.Local.Store.Pins()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pins {
		if p.CID == root && p.Source == store.SourceExplicit {
			if _, err := nd.Local.Store.Unpin(p); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func memberState(nd *nutest.Node, id *nutest.Node) string {
	m := nd.Local.NetCluster().View().Members[id.ID]
	if m.Value == nil {
		return ""
	}
	return m.Value.State
}

// A storage member dies (0048 acceptance, R20). Without an online repair authority its
// reports go stale: readiness is unknown, never degraded, nothing is re-placed and status
// alerts that repair is unavailable. With an authority online, the member is marked
// unavailable: readiness is degraded (not unknown) while capacity blocks re-replication,
// and once capacity frees, survivors restore k copies from each other. When the member
// returns, the authority marks it active again. On the way the writer releases its
// surplus copies by explicit gc once their owners confirm them (S2).
func TestRepairAfterLoss(t *testing.T) {
	tune(t, func(*node.ClusterSettings) {})
	n := nutest.New(t, 4)
	a, b, c, d := n.Nodes[0], n.Nodes[1], n.Nodes[2], n.Nodes[3]
	storageCluster(t, a, b, c, d)
	if _, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "photos", Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	root := addDir(t, n, a, 12)
	if _, err := a.Local.DatasetUpdate(ctx(t), node.ScopeCluster, "photos", apiv0.DatasetUpdateRequest{Target: root.String()}); err != nil {
		t.Fatal(err)
	}
	if r, err := b.Local.DatasetWait(ctx(t), node.ScopeCluster, "photos", apiv0.DatasetWaitRequest{Target: root.String(), Timeout: "40s"}); err != nil || !r.Satisfied {
		t.Fatalf("ready: %+v %v", r, err)
	}

	// The writer keeps only what it owns: its other copies are surplus, released by an
	// explicit gc once every owner confirms them.
	unpinExplicit(t, a, root)
	var rel *apiv0.ReleaseReport
	eventually(t, "a's surplus releasable", func() bool {
		r, err := a.Local.DatasetRelease(ctx(t), node.ScopeCluster, "photos", apiv0.ReleaseRequest{DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		rel = r
		ok := len(r.Deferred) == 0
		for _, cand := range r.Candidates {
			ok = ok && (cand.Kind != "surplus" || cand.OK)
		}
		return ok && slices.ContainsFunc(r.Candidates, func(c apiv0.ReleaseCandidate) bool { return c.Kind == "surplus" })
	})
	if !rel.DryRun || rel.ReleasedBlobs != 0 {
		t.Fatalf("dry run released: %+v", rel)
	}
	rel, err := a.Local.DatasetRelease(ctx(t), node.ScopeCluster, "photos", apiv0.ReleaseRequest{})
	if err != nil || rel.ReleasedBlobs == 0 {
		t.Fatalf("gc: %+v %v", rel, err)
	}
	v := a.Local.NetCluster().View()
	ds, _ := v.Dataset("photos")
	pl := cluster.NewPlacement(v, ds.Policy, a.ID)
	for _, bl := range blobsOf(t, b, root) {
		if pl.Owns(a.ID, bl.Multihash()) != holds(a, bl) {
			t.Fatalf("after gc a holds %v of blob %s it owns: %v", holds(a, bl), bl, pl.Owns(a.ID, bl.Multihash()))
		}
	}
	eventually(t, "ready on owners alone", func() bool { return content(t, b, "photos").State == cluster.ReadyReady })

	// No repair authority: d dies; evidence goes stale; unknown with an alert, no repair.
	d.Kill()
	eventually(t, "unknown without authority", func() bool {
		cs := content(t, b, "photos")
		if cs.State == cluster.ReadyDegraded {
			t.Fatalf("degraded from stale evidence alone: %+v", cs)
		}
		return cs.State == cluster.ReadyUnknown && hasAlert(cs.Alerts, apiv0.AlertNoRepairAuthority) && cs.Repair == "unavailable"
	})
	st, err := b.Local.ClusterStatus(ctx(t))
	if err != nil || !hasAlert(st.Network.Alerts, apiv0.AlertNoRepairAuthority) || st.Network.Repair.Online {
		t.Fatalf("cluster status without authority: %+v %v", st.Network, err)
	}
	time.Sleep(2 * time.Second) // well past down_after: still nobody marks d
	if s := memberState(b, d); s != cluster.StateActive {
		t.Fatalf("d marked %s without an authority", s)
	}

	// Survivors have no capacity for another blob (signed state still fits); a restarts
	// as a repair authority.
	full := func(nd *nutest.Node) {
		u, err := nd.Local.Store.Usage()
		if err != nil {
			t.Fatal(err)
		}
		nd.Local.Store.SetLimits(store.Limits{Physical: u + 8<<10, FreeReserve: 0})
	}
	full(b)
	full(c)
	a.Stop()
	if err := a.Repo.ConfigSet("cluster.repair", "auto"); err != nil {
		t.Fatal(err)
	}
	a.Start()
	full(a)
	restarted := time.Now()
	eventually(t, "d marked unavailable", func() bool { return memberState(b, d) == cluster.StateUnavailable })
	if time.Since(restarted) < time.Second {
		t.Fatalf("marked %s after the authority started, before down_after", time.Since(restarted))
	}
	if m := b.Local.NetCluster().View().Members[d.ID]; !m.Value.Auto {
		t.Fatal("repair record not marked auto")
	}
	eventually(t, "degraded while blocked", func() bool {
		cs := content(t, b, "photos")
		return cs.State == cluster.ReadyDegraded && cs.Repair == "in_progress" && hasAlert(cs.Alerts, apiv0.AlertDegraded) && cs.Copies != nil && cs.Copies.Below > 0
	})
	time.Sleep(500 * time.Millisecond)
	if cs := content(t, b, "photos"); cs.State != cluster.ReadyDegraded {
		t.Fatalf("blocked repair reported %s", cs.State)
	}

	// Capacity frees: survivors restore k copies from each other.
	for _, nd := range []*nutest.Node{a, b, c} {
		nd.Local.Store.SetLimits(store.Limits{})
	}
	eventually(t, "k copies restored", func() bool {
		cs := content(t, b, "photos")
		return cs.State == cluster.ReadyReady && cs.Copies.Min >= 2 &&
			!slices.ContainsFunc(cs.Confirmed, func(c apiv0.Confirmation) bool { return c.Peer == d.ID.Base36() })
	})

	// d returns: heartbeats for up_after make it active again.
	d.Start()
	eventually(t, "d active again", func() bool { return memberState(b, d) == cluster.StateActive })
}

// pinned reports whether nd keeps root pinned for dataset ds.
func pinned(t *testing.T, nd *nutest.Node, ds string, root cid.CID) bool {
	t.Helper()
	pins, err := nd.Local.Store.Pins()
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(pins, func(p store.Pin) bool {
		return p.CID == root && p.Source == store.SourceCluster && p.Name == "dataset/"+ds
	})
}

func update(t *testing.T, nd *nutest.Node, ds string, root cid.CID) {
	t.Helper()
	if _, err := nd.Local.DatasetUpdate(ctx(t), node.ScopeCluster, ds, apiv0.DatasetUpdateRequest{Target: root.String()}); err != nil {
		t.Fatal(err)
	}
}

func waitReady(t *testing.T, nd *nutest.Node, ds string, root cid.CID) {
	t.Helper()
	if r, err := nd.Local.DatasetWait(ctx(t), node.ScopeCluster, ds, apiv0.DatasetWaitRequest{Target: root.String(), Timeout: "40s"}); err != nil || !r.Satisfied {
		t.Fatalf("ready %s: %+v %v", root, r, err)
	}
}

func dryRun(t *testing.T, nd *nutest.Node, ds string) *apiv0.ReleaseReport {
	t.Helper()
	r, err := nd.Local.DatasetRelease(ctx(t), node.ScopeCluster, ds, apiv0.ReleaseRequest{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func deferredBy(r *apiv0.ReleaseReport, prefix string) bool {
	return slices.ContainsFunc(r.Deferred, func(d string) bool { return len(d) >= len(prefix) && d[:len(prefix)] == prefix })
}

// Automatic release (release.auto) under S1–S4: an old head is released on every member
// once owners confirm the new one and the binding settled; a partition that hides an
// owner keeps the old head pinned everywhere and the dry run names the stale owner
// (R10, R11); an epoch change defers release until it settles (R12); concurrent heads
// from two writers defer every release and both writers see the displaced head (R14).
func TestSafeRelease(t *testing.T) {
	tune(t, func(s *node.ClusterSettings) { s.ReleaseAuto, s.Settle = true, 2*time.Second })
	n := nutest.New(t, 3)
	a, b, c := n.Nodes[0], n.Nodes[1], n.Nodes[2]
	all := []*nutest.Node{a, b, c}
	storageCluster(t, a, b, c)
	if _, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "docs", Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	t1 := addDir(t, n, a, 6)
	update(t, a, "docs", t1)
	waitReady(t, b, "docs", t1)
	unpinExplicit(t, a, t1)
	t2 := addDir(t, n, a, 6)
	update(t, a, "docs", t2)
	waitReady(t, b, "docs", t2)
	unpinExplicit(t, a, t2)
	eventually(t, "t1 released everywhere", func() bool {
		for _, nd := range all {
			if pinned(t, nd, "docs", t1) {
				return false
			}
		}
		return true
	})

	// R10/R11: c is partitioned off after the next head; nobody releases t2.
	n.Partition(a, c)
	n.Partition(b, c)
	t3 := addDir(t, n, a, 6)
	update(t, a, "docs", t3)
	deadline := time.Now().Add(node.EvidenceFresh + 3*time.Second)
	for time.Now().Before(deadline) {
		for _, nd := range all {
			if !pinned(t, nd, "docs", t2) {
				t.Fatalf("%s released t2 while an owner was partitioned", nd.Name)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	r := dryRun(t, a, "docs")
	i := slices.IndexFunc(r.Candidates, func(c apiv0.ReleaseCandidate) bool { return c.CID == t2.String() })
	if i < 0 || r.Candidates[i].OK || !slices.ContainsFunc(r.Candidates[i].Shortfall, func(m apiv0.MissingOwner) bool {
		return m.Peer == c.ID.Base36() && (m.Reason == cluster.ReasonStale || m.Reason == cluster.ReasonAbsent)
	}) {
		t.Fatalf("R11 dry run: %+v", r)
	}
	n.Heal(a, c)
	n.Heal(b, c)
	waitReady(t, b, "docs", t3)
	unpinExplicit(t, a, t3)
	eventually(t, "t2 released after healing", func() bool {
		for _, nd := range all {
			if pinned(t, nd, "docs", t2) {
				return false
			}
		}
		return true
	})

	// R12: an epoch change right after the next head became ready defers release.
	t4 := addDir(t, n, a, 6)
	update(t, a, "docs", t4)
	waitReady(t, a, "docs", t4)
	if _, err := a.Local.ClusterMemberSet(ctx(t), apiv0.MemberSetRequest{Peer: b.ID.String(), Capacity: 2 << 40}); err != nil {
		t.Fatal(err)
	}
	if r := dryRun(t, a, "docs"); !deferredBy(r, "unsettled") || !pinned(t, a, "docs", t3) {
		t.Fatalf("R12: release not deferred after an epoch change: %+v", r)
	}
	waitReady(t, a, "docs", t4)
	eventually(t, "t3 released once the epoch settled", func() bool {
		for _, nd := range all {
			if pinned(t, nd, "docs", t3) {
				return false
			}
		}
		return true
	})

	// R14: b becomes a second admin; a and b write heads while partitioned.
	k, err := b.Local.ClusterKey(ctx(t), apiv0.KeyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Local.ClusterKey(ctx(t), apiv0.KeyRequest{DID: k.DID, Role: cluster.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	converged(t, n, "second admin", func(v *cluster.View) bool { return v.Keys[k.DID] != nil }, a, b, c)
	n.Partition(a, b)
	n.Partition(b, c)
	t5a, t5b := addDir(t, n, a, 3), addDir(t, n, b, 3)
	update(t, a, "docs", t5a)
	update(t, b, "docs", t5b)
	n.Heal(a, b)
	n.Heal(b, c)
	eventually(t, "both writers see the displaced head", func() bool {
		for _, nd := range []*nutest.Node{a, b} {
			d, err := nd.Local.Dataset(ctx(t), node.ScopeCluster, "docs")
			if err != nil || len(d.Displaced) != 1 || len(d.Heads) != 2 {
				return false
			}
		}
		return true
	})
	if r := dryRun(t, a, "docs"); !deferredBy(r, "concurrent_heads") {
		t.Fatalf("R14: concurrent heads not deferred: %+v", r)
	}
	for _, nd := range all {
		if !pinned(t, nd, "docs", t4) {
			t.Fatalf("R14: %s released the previous head during concurrent heads", nd.Name)
		}
	}
	// Key revocation (0069 CLI/API): b's key is frozen at its last commit.
	if _, err := a.Local.ClusterKeyRevoke(ctx(t), k.DID); err != nil {
		t.Fatal(err)
	}
	converged(t, n, "revocation", func(v *cluster.View) bool { return v.Keys[k.DID] == nil || v.Keys[k.DID].Revoked != nil }, b, c)
}

func isolate(n *nutest.Network, x *nutest.Node, others ...*nutest.Node) {
	for _, o := range others {
		n.Partition(x, o)
	}
}

func rejoin(n *nutest.Network, x *nutest.Node, others ...*nutest.Node) {
	for _, o := range others {
		n.Heal(x, o)
	}
}

// Drain and remove (S6, R16): `member rm` of an active owner is refused (drain first);
// a drained member is not removable while an owner of its blobs is partitioned (the
// result lists the unconfirmed content); after healing, the drained member releases its
// copies once the new owners confirm them and the removal is safe; `--force` removes
// anyway and reports what it left unconfirmed.
func TestDrainRemove(t *testing.T) {
	tune(t, func(s *node.ClusterSettings) { s.ReleaseAuto, s.Settle = true, 500*time.Millisecond })
	n := nutest.New(t, 4)
	a, b, c, d := n.Nodes[0], n.Nodes[1], n.Nodes[2], n.Nodes[3]
	storageCluster(t, a, b, c, d)
	if _, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "logs", Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	root := addDir(t, n, a, 10)
	update(t, a, "logs", root)
	waitReady(t, b, "logs", root)
	unpinExplicit(t, a, root)
	rm := func(id *nutest.Node, dry, force bool) (*apiv0.MemberRemoval, error) {
		return a.Local.ClusterMemberRemove(ctx(t), apiv0.ClusterMemberRemoveRequest{Peer: id.ID.String(), DryRun: dry, Force: force})
	}
	if r, err := rm(b, true, false); err != nil || r.Safe || len(r.Reasons) == 0 {
		t.Fatalf("rm of an active owner: %+v %v", r, err)
	}
	// Invite revocation (0069 CLI/API).
	inv, err := a.Local.ClusterInvite(ctx(t), apiv0.ClusterInviteRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Local.ClusterInviteRevoke(ctx(t), inv.InviteID); err != nil {
		t.Fatal(err)
	}
	if _, live := a.Local.NetCluster().View().LiveInvite(inv.InviteID, time.Now()); live {
		t.Fatal("revoked invite still live")
	}

	isolate(n, c, a, b, d)
	if _, err := a.Local.ClusterMemberSet(ctx(t), apiv0.MemberSetRequest{Peer: d.ID.String(), State: cluster.StateDraining}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(node.EvidenceFresh + time.Second)
	r, err := rm(d, false, false)
	if waitCode(err) != apiv0.CodeFailed || r == nil || r.Removed || len(r.Unconfirmed) == 0 ||
		!slices.ContainsFunc(r.Unconfirmed[0].Missing, func(m apiv0.MissingOwner) bool { return m.Peer == c.ID.Base36() }) {
		t.Fatalf("R16: rm while an owner is partitioned: %+v %v", r, err)
	}
	if !pinned(t, d, "logs", root) {
		t.Fatal("the draining member released its view while an owner is partitioned")
	}

	rejoin(n, c, a, b, d)
	eventually(t, "drain confirmed", func() bool {
		r, err := rm(d, true, false)
		return err == nil && r.Safe
	})
	eventually(t, "d released its copies", func() bool { return !pinned(t, d, "logs", root) })
	if r, err := rm(d, false, false); err != nil || !r.Removed || r.Forced {
		t.Fatalf("rm after drain: %+v %v", r, err)
	}

	// --force: c drains while b, an owner, is partitioned.
	converged(t, n, "d removed", func(v *cluster.View) bool { return !v.IsMember(d.ID) }, a, b, c)
	isolate(n, b, a, c)
	if _, err := a.Local.ClusterMemberSet(ctx(t), apiv0.MemberSetRequest{Peer: c.ID.String(), State: cluster.StateDraining}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(node.EvidenceFresh + time.Second)
	r, err = rm(c, false, true)
	if err != nil || !r.Removed || !r.Forced || len(r.Unconfirmed) == 0 {
		t.Fatalf("forced rm: %+v %v", r, err)
	}
}

// Audits and scrub (E9, R18): a member whose report is incomplete passes a sampled
// audit, yet is not confirmed (the audit is reported separately); a scrub finds a
// corrupted blob file, drops it, and the member fetches it again.
func TestAuditScrub(t *testing.T) {
	tune(t, func(*node.ClusterSettings) {})
	n := nutest.New(t, 3)
	a, b, c := n.Nodes[0], n.Nodes[1], n.Nodes[2]
	storageCluster(t, a, b, c)
	if _, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "all", All: true}); err != nil {
		t.Fatal(err)
	}
	root := addDir(t, n, a, 4)
	lost := blobsOf(t, a, root)[0]
	if _, err := a.Local.Store.Drop(ctx(t), []cid.Multihash{lost.Multihash()}, func(store.Pin) bool { return false }); err != nil {
		t.Fatal(err)
	}
	update(t, a, "all", root)
	eventually(t, "c holds every blob but the lost one", func() bool {
		held := 0
		for _, bl := range blobsOf(t, a, root) {
			if holds(c, bl) {
				held++
			}
		}
		return held == 3
	})
	rep, err := a.Local.ClusterAudit(ctx(t), apiv0.AuditRequest{Peer: c.ID.String(), Samples: 12})
	if err != nil || !slices.ContainsFunc(rep.Challenges, func(ch apiv0.Challenge) bool { return ch.OK }) {
		t.Fatalf("audit of c: %+v %v", rep, err)
	}
	cs := content(t, a, "all")
	if cs.State == cluster.ReadyReady || slices.ContainsFunc(cs.Confirmed, func(x apiv0.Confirmation) bool { return x.Peer == c.ID.Base36() }) {
		t.Fatalf("R18: a passed audit counted as confirmation: %+v", cs)
	}
	st, err := a.Local.ClusterStatus(ctx(t))
	if err != nil || !slices.ContainsFunc(st.Network.Audits, func(p apiv0.AuditPeer) bool { return p.Peer == c.ID.Base36() && p.Passed > 0 }) {
		t.Fatalf("audits not shown: %+v %v", st, err)
	}

	// Bit rot on b: corrupt one stored blob file; a full scrub drops it and b re-fetches.
	var victim string
	_ = filepath.WalkDir(b.Dir, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && victim == "" && filepath.Base(filepath.Dir(filepath.Dir(p))) == "blobs" && !strings.HasSuffix(p, ".obao") {
			victim = p
		}
		return nil
	})
	if victim == "" {
		t.Fatal("no blob file on b")
	}
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(victim, data, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err = b.Local.ClusterAudit(ctx(t), apiv0.AuditRequest{Full: true, Peer: a.ID.String()})
	if err != nil || rep.Scrub == nil || len(rep.Scrub.Corrupt) != 1 {
		t.Fatalf("scrub: %+v %v", rep, err)
	}
	bad, _ := cid.Parse(rep.Scrub.Corrupt[0])
	if st, err := b.Local.ClusterStatus(ctx(t)); err != nil || !hasAlert(st.Network.Alerts, apiv0.AlertScrubFailed) {
		t.Fatalf("scrub alert: %+v %v", st, err)
	}
	eventually(t, "b re-fetched the corrupt blob", func() bool {
		ok, err := b.Local.Store.Verify(bad)
		return holds(b, bad) && ok && err == nil
	})
}
