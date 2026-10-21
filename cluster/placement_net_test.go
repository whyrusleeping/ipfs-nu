package cluster_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/nutest"
)

// storageCluster founds a private cluster on nodes[0] with every other node as a
// pre-authorized storage member.
func storageCluster(t *testing.T, nodes ...*nutest.Node) {
	t.Helper()
	a := nodes[0]
	if _, err := a.Local.ClusterInit(ctx(t), apiv0.ClusterInitRequest{Name: "lab"}); err != nil {
		t.Fatal(err)
	}
	for _, nd := range nodes[1:] {
		inv, err := a.Local.ClusterInvite(ctx(t), apiv0.ClusterInviteRequest{Peer: nd.ID.String()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := nd.Local.ClusterJoin(ctx(t), apiv0.ClusterJoinRequest{Token: inv.Token}); err != nil {
			t.Fatal(err)
		}
	}
	nutest.Eventually(t, "membership", func() bool {
		for _, nd := range nodes {
			if len(nd.Local.NetCluster().View().MemberIDs()) != len(nodes) {
				return false
			}
		}
		return true
	})
}

// addDir adds a directory of n random files.
func addDir(t *testing.T, n *nutest.Network, nd *nutest.Node, files int) cid.CID {
	t.Helper()
	dir := t.TempDir()
	for i := range files {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d", i)), n.RandBytes(20<<10), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return nd.Add(dir, true)
}

// blobsOf lists the blobs of a held tree.
func blobsOf(t *testing.T, nd *nutest.Node, root cid.CID) []cid.CID {
	t.Helper()
	var out []cid.CID
	stack := []cid.CID{root}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if c.Codec() == cid.Raw {
			out = append(out, c)
			continue
		}
		data, err := nd.Local.Store.Get(c)
		if err != nil {
			t.Fatal(err)
		}
		links, err := fetch.Links(c, data)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range links {
			stack = append(stack, l.CID)
		}
	}
	return out
}

func holds(nd *nutest.Node, c cid.CID) bool {
	info, ok, err := nd.Local.Store.Has(c)
	return err == nil && ok && info.Complete
}

func waitCode(err error) apiv0.Code {
	var ae *apiv0.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// Placement and readiness on three routed private-mode storage members (0046): a head
// write's metadata acknowledgment is not content readiness (R1); members fetch their own
// blobs by placement and the target becomes ready only from every owner's fresh report;
// a wait stays on its exact target when the head moves (R3); an infeasible policy fails a
// ready wait fast (R6); held waits name holders.
func TestPlacementReadiness(t *testing.T) {
	n := nutest.New(t, 4)
	a, b, c, outsider := n.Nodes[0], n.Nodes[1], n.Nodes[2], n.Nodes[3]
	storageCluster(t, a, b, c)
	if _, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "photos", Replicas: 2}); err != nil {
		t.Fatal(err)
	}

	// R1: a head whose content no member can get is acknowledged as metadata only.
	lost := outsider.AddBytes(n.RandBytes(50 << 10))
	d, err := a.Local.DatasetUpdate(ctx(t), node.ScopeCluster, "photos", apiv0.DatasetUpdateRequest{Target: lost.String(), Kind: "file"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Metadata.Acknowledged || d.Content.State == cluster.ReadyReady || d.Content.Target != lost.String() {
		t.Fatalf("head write result %+v", d)
	}
	md, err := a.Local.DatasetWait(ctx(t), node.ScopeCluster, "photos", apiv0.DatasetWaitRequest{Target: lost.String(), Condition: apiv0.WaitMetadata, Acks: 3})
	if err != nil || !md.Satisfied || len(md.Metadata.AckedBy) != 3 {
		t.Fatalf("metadata wait %+v, %v", md, err)
	}
	res, err := a.Local.DatasetWait(ctx(t), node.ScopeCluster, "photos", apiv0.DatasetWaitRequest{Target: lost.String(), Timeout: "300ms"})
	if waitCode(err) != apiv0.CodeTimeout || res == nil || res.Satisfied || res.State == cluster.ReadyReady {
		t.Fatalf("ready wait on unavailable content: %+v, %v", res, err)
	}

	// Content held by the writer: every owner fetches its blobs and reports; ready.
	t1 := addDir(t, n, a, 16)
	if _, err := a.Local.DatasetUpdate(ctx(t), node.ScopeCluster, "photos", apiv0.DatasetUpdateRequest{Target: t1.String()}); err != nil {
		t.Fatal(err)
	}
	res, err = c.Local.DatasetWait(ctx(t), node.ScopeCluster, "photos", apiv0.DatasetWaitRequest{Target: t1.String(), Timeout: "10s"})
	if err != nil || !res.Satisfied || res.State != cluster.ReadyReady || len(res.Confirmed) != res.Required || res.Required < 2 {
		t.Fatalf("ready wait: %+v, %v", res, err)
	}
	// Each member holds the structure and exactly the blobs it owns (k=2).
	v := b.Local.NetCluster().View()
	ds, _ := v.Dataset("photos")
	pl := cluster.NewPlacement(v, ds.Policy, b.ID)
	for _, bl := range blobsOf(t, a, t1) {
		owners := pl.Owners(bl.Multihash())
		if len(owners) != 2 {
			t.Fatalf("owners %v", owners)
		}
		for _, nd := range []*nutest.Node{b, c} {
			if own := slices.Contains(owners, nd.ID); own != holds(nd, bl) {
				t.Fatalf("%s: owns %v, holds %v blob %s", nd.Name, own, holds(nd, bl), bl)
			}
		}
	}
	// held: b's report is complete for t1.
	if res, err := a.Local.DatasetWait(ctx(t), node.ScopeCluster, "photos", apiv0.DatasetWaitRequest{Target: t1.String(), Condition: apiv0.WaitHeld,
		Holders: []string{b.ID.String()}, Timeout: "10s"}); err != nil || !res.Satisfied {
		t.Fatalf("held wait: %+v, %v", res, err)
	}
	if _, err := a.Local.DatasetWait(ctx(t), node.ScopeCluster, "photos", apiv0.DatasetWaitRequest{Target: t1.String(), Condition: apiv0.WaitHeld,
		Holders: []string{outsider.ID.String()}}); waitCode(err) != apiv0.CodeFailed {
		t.Fatalf("held wait on a non-member: %v", err)
	}

	// R3: the head moves while a wait for an unavailable target runs; the wait keeps its
	// target and reports the new head, which becomes ready on its own.
	t2 := outsider.AddBytes(n.RandBytes(30 << 10))
	if _, err := a.Local.DatasetUpdate(ctx(t), node.ScopeCluster, "photos", apiv0.DatasetUpdateRequest{Target: t2.String(), Kind: "file"}); err != nil {
		t.Fatal(err)
	}
	type out struct {
		res *apiv0.WaitResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := b.Local.DatasetWait(context.Background(), node.ScopeCluster, "photos", apiv0.DatasetWaitRequest{Target: t2.String(), Timeout: "3s"})
		done <- out{r, err}
	}()
	t3 := addDir(t, n, a, 4)
	if _, err := a.Local.DatasetUpdate(ctx(t), node.ScopeCluster, "photos", apiv0.DatasetUpdateRequest{Target: t3.String()}); err != nil {
		t.Fatal(err)
	}
	if r, err := a.Local.DatasetWait(ctx(t), node.ScopeCluster, "photos", apiv0.DatasetWaitRequest{Target: t3.String(), Timeout: "10s"}); err != nil || !r.Satisfied {
		t.Fatalf("new head not ready: %+v, %v", r, err)
	}
	o := <-done
	if waitCode(o.err) != apiv0.CodeTimeout || o.res.Satisfied || o.res.Target != t2.String() || o.res.SupersededBy != t3.String() {
		t.Fatalf("superseded wait: %+v, %v", o.res, o.err)
	}

	// R6: replicas 4 on three storage members fails fast and names the shortfall.
	if _, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "wide", Replicas: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Local.DatasetUpdate(ctx(t), node.ScopeCluster, "wide", apiv0.DatasetUpdateRequest{Target: t3.String()}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r, err := a.Local.DatasetWait(ctx(t), node.ScopeCluster, "wide", apiv0.DatasetWaitRequest{Target: t3.String()})
	if waitCode(err) != apiv0.CodeFailed || r.State != cluster.ReadyUnsatisfied || len(r.Unmet) == 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("infeasible wait: %+v, %v", r, err)
	}
	// R6 with spread: no member is zone-tagged, so a zone spread is unsatisfiable.
	zd, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "zoned", Replicas: 2, Spread: "zone"})
	if err != nil || zd.Spread != "zone" {
		t.Fatalf("zoned dataset %+v, %v", zd, err)
	}
	if _, err := a.Local.DatasetUpdate(ctx(t), node.ScopeCluster, "zoned", apiv0.DatasetUpdateRequest{Target: t3.String()}); err != nil {
		t.Fatal(err)
	}
	r, err = a.Local.DatasetWait(ctx(t), node.ScopeCluster, "zoned", apiv0.DatasetWaitRequest{Target: t3.String()})
	if waitCode(err) != apiv0.CodeFailed || r.State != cluster.ReadyUnsatisfied || !strings.Contains(strings.Join(r.Unmet, ";"), `"zone"`) {
		t.Fatalf("spread wait: %+v, %v", r, err)
	}
	// The team default warns instead of silently sharing failure domains.
	if wd, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "plain"}); err != nil || wd.Spread != "" || len(wd.Warnings) == 0 || wd.Replicas != 2 {
		t.Fatalf("default dataset %+v, %v", wd, err)
	}

}

// The one-node local cluster: readiness is local completeness at the explicit k=1, and
// never claims a second copy (R9 lands fully with 0047's presets).
func TestLocalReady(t *testing.T) {
	n := nutest.New(t, 1)
	a := n.Nodes[0]
	if _, err := a.Local.DatasetCreate(ctx(t), node.ScopeLocal, apiv0.DatasetCreateRequest{Name: "notes"}); err != nil {
		t.Fatal(err)
	}
	root := addDir(t, n, a, 3)
	d, err := a.Local.DatasetUpdate(ctx(t), node.ScopeLocal, "notes", apiv0.DatasetUpdateRequest{Target: root.String()})
	if err != nil || !d.Metadata.Acknowledged || d.Replicas != 1 {
		t.Fatalf("update %+v, %v", d, err)
	}
	r, err := a.Local.DatasetWait(ctx(t), node.ScopeLocal, "notes", apiv0.DatasetWaitRequest{Target: root.String(), Timeout: "5s"})
	if err != nil || r.State != cluster.ReadyReady || r.Required != 1 || len(r.Confirmed) != 1 || r.Epoch != cluster.EpochLocal {
		t.Fatalf("local wait %+v, %v", r, err)
	}
}
