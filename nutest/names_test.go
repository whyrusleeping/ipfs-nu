package nutest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/nutest"
	"github.com/whyrusleeping/nu/nutest/atfix"
	"github.com/whyrusleeping/nu/store"
)

// The publish → exact-seed wait → laptop sleep journey (0045; docs/follow.md "Seed
// waits", operations W1–W5, R2, R19). The publisher a runs a public-mode cluster with
// seed b as its storage member and logs in to the fixture account. `publish --wait-seed b`
// writes the record, creates the seed-layout cluster follow and waits for b's fresh
// complete report for exactly the published CID: while b's store budget stops it short of
// all blobs, the wait is unmet with b listed `incomplete` (R2) although b holds the head
// record and structure; once b completes, the wait is satisfied and the publication and
// wait are separate results (W4). a then stops (W5) and a fresh third node fetches the
// CID from b alone (R19). A later publication moves the follow without the old wait ever
// retargeting (W1).
func TestPublishSeedWait(t *testing.T) {
	old := node.ClusterInterval
	node.ClusterInterval = 200 * time.Millisecond
	defer func() { node.ClusterInterval = old }()
	f := atfix.New(t)
	n := nutest.New(t, 2)
	a, b := n.Nodes[0], n.Nodes[1]
	ctx := context.Background()
	if _, err := a.Local.ClusterInit(ctx, apiv0.ClusterInitRequest{Name: "lab", Mode: "public"}); err != nil {
		t.Fatal(err)
	}
	inv, err := a.Local.ClusterInvite(ctx, apiv0.ClusterInviteRequest{Peer: b.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Local.ClusterJoin(ctx, apiv0.ClusterJoinRequest{Token: inv.Token}); err != nil {
		t.Fatal(err)
	}
	nutest.Eventually(t, "membership", func() bool {
		return len(a.Local.NetCluster().View().MemberIDs()) == 2 && len(b.Local.NetCluster().View().MemberIDs()) == 2
	})
	a.Local.Atproto = atproto.New(f.Config())
	start, err := a.Local.Login(ctx, apiv0.LoginRequest{Identifier: atfix.PubHandle})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.Browser().Get(start.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if final, err := a.Local.WaitOperation(ctx, start.Operation.ID, 0); err != nil || final.State != apiv0.StateSucceeded {
		t.Fatalf("login: %v %+v", err, final)
	}

	dir := t.TempDir()
	for i := range 6 {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), n.RandBytes(50<<10), 0o644)
	}
	root := a.Add(dir, true)

	// R2: the seed cannot fit the blobs; the wait times out with b incomplete, and the
	// publication itself succeeded regardless.
	b.Local.Store.SetLimits(store.Limits{Physical: 200 << 10})
	res, err := a.Local.Publish(ctx, apiv0.PublishRequest{Name: "photos", Target: root.String(), WaitSeed: []string{b.ID.String()}, Timeout: "4s"})
	if err == nil || res == nil || res.Wait == nil || res.Wait.Satisfied {
		t.Fatalf("budget-limited seed wait: %v %+v", err, res)
	}
	var ae *apiv0.Error
	if !errorsAs(err, &ae) || ae.Code != apiv0.CodeTimeout || res.Publication.Rev == "" || res.Publication.Dataset != "pub-photos" {
		t.Fatalf("R2: %v %+v", err, res.Publication)
	}
	if len(res.Wait.Missing) != 1 || res.Wait.Missing[0].Reason != "incomplete" || res.Wait.Target != root.String() {
		bd, berr := b.Local.Dataset(ctx, node.ScopeCluster, "pub-photos")
		t.Logf("seed dataset: %+v %v", bd, berr)
		bl, _ := b.Local.Datasets(ctx, node.ScopeCluster)
		al, _ := a.Local.Datasets(ctx, node.ScopeCluster)
		t.Logf("b datasets %+v\na datasets %+v", bl, al)
		cs, _ := a.Local.ClusterStatus(ctx)
		t.Logf("a cluster status %+v", cs)
		if bd != nil && bd.Content.Local != nil {
			t.Logf("seed local: %+v", *bd.Content.Local)
		}
		t.Fatalf("R2 missing: %+v", res.Wait)
	}
	ds, err := b.Local.Dataset(ctx, node.ScopeCluster, "pub-photos")
	if err != nil || ds.Head == nil || ds.Head.Target != root.String() || ds.Content.Local == nil || ds.Content.Local.Complete {
		t.Fatalf("seed sees the head but is not complete: %+v %v", ds, err)
	}

	// The seed gets room: the same exact-target wait is satisfied; results stay separate.
	b.Local.Store.SetLimits(store.Limits{})
	res, err = a.Local.Publish(ctx, apiv0.PublishRequest{Name: "photos", Target: root.String(), WaitSeed: []string{b.ID.String()}, Timeout: "30s"})
	if err != nil || !res.Wait.Satisfied || res.Wait.Target != root.String() || res.Wait.Condition != apiv0.WaitHeld || len(res.Wait.Confirmed) != 1 {
		t.Fatalf("seed wait: %v %+v", err, res)
	}
	fl, _ := b.Local.Follows(ctx)
	if len(fl) != 1 || fl[0].State != "held" || fl[0].Held.Target != root.String() || fl[0].Content.State != "ready" || len(fl[0].Content.Confirmed) != fl[0].Content.Required {
		t.Fatalf("seed follow ls: %+v", fl)
	}

	// W1: a newer publication during a wait for the old target never retargets it.
	os.WriteFile(filepath.Join(dir, "f0"), n.RandBytes(50<<10), 0o644)
	root2 := a.Add(dir, true)
	n.Partition(a, b)
	waitErr := make(chan error, 1)
	go func() {
		_, err := a.Local.DatasetWait(ctx, node.ScopeCluster, "pub-photos", apiv0.DatasetWaitRequest{Target: root2.String(), Condition: apiv0.WaitHeld, Holders: []string{b.ID.String()}, Timeout: "2s"})
		waitErr <- err
	}()
	if _, err := a.Local.Publish(ctx, apiv0.PublishRequest{Name: "photos", Target: root2.String()}); err != nil {
		t.Fatal(err)
	}
	if err := <-waitErr; !errorsAs(err, &ae) || ae.Code != apiv0.CodeTimeout {
		t.Fatalf("partitioned wait: %v", err)
	}
	n.Heal(a, b)
	if res, err := a.Local.Publish(ctx, apiv0.PublishRequest{Name: "photos", Target: root2.String(), WaitSeed: []string{b.ID.String()}, Timeout: "30s"}); err != nil || !res.Wait.Satisfied {
		t.Fatalf("second seed wait: %v %+v", err, res)
	}

	// R19 / W5: the publisher goes to sleep; a fresh third node fetches from the seed.
	a.Stop()
	fresh := n.NewNode(false)
	out := filepath.Join(t.TempDir(), "out")
	got, err := fresh.TryGet(n.Link(root2, b), out)
	if err != nil || got.CID != root2.String() {
		t.Fatalf("fresh get from the seed alone: %v %+v", err, got)
	}
	if data, _ := os.ReadFile(filepath.Join(out, "f1")); len(data) != 50<<10 {
		t.Fatalf("fetched %d bytes", len(data))
	}
}

func errorsAs(err error, target **apiv0.Error) bool {
	for err != nil {
		if e, ok := err.(*apiv0.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
