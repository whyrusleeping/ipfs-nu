package node_test

import (
	"context"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/nutest"
	"github.com/whyrusleeping/nu/nutest/atfix"
)

// An atrepo dataset on a networked cluster (0054): the admin fetches from the PDS and
// writes the head with its rev; the signed state carries head, rev and account status to
// the storage member, which pins under the atrepo profile and reports honestly that it
// does not hold the repository (member replication over /nu/tree/1 atrepo diffs is not
// implemented yet), never that it is complete. Updates and the status flip propagate.
func TestAtrepoClusterDataset(t *testing.T) {
	f := atfix.New(t)
	old := node.ClusterInterval
	node.ClusterInterval = 200 * time.Millisecond
	defer func() { node.ClusterInterval = old }()
	nw := nutest.New(t, 2)
	a, b := nw.Nodes[0], nw.Nodes[1]
	a.Local.Atproto = atproto.New(f.Config())
	b.Local.Atproto = atproto.New(f.Config())
	ctx, cancel := context.WithTimeout(context.Background(), 4*nutest.Timeout)
	defer cancel()
	if _, err := a.Local.ClusterInit(ctx, apiv0.ClusterInitRequest{Name: "lab"}); err != nil {
		t.Fatal(err)
	}
	inv, err := a.Local.ClusterInvite(ctx, apiv0.ClusterInviteRequest{Peer: b.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Local.ClusterJoin(ctx, apiv0.ClusterJoinRequest{Token: inv.Token}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "membership", func() bool {
		return len(a.Local.NetCluster().View().MemberIDs()) == 2 && len(b.Local.NetCluster().View().MemberIDs()) == 2
	})
	pub := f.Pub
	pub.Put("app.bsky.feed.post", "3aaa", map[string]any{"$type": "app.bsky.feed.post", "text": "hello", "createdAt": "2026-01-01T00:00:00Z"})

	d, err := a.Local.DatasetCreate(ctx, node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "pub", Archive: true, SourceAt: "at://" + atfix.PubDID, Replicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	if d.Head == nil || d.Head.Rev != pub.Rev() || len(d.Warnings) != 1 { // no zone spread
		t.Fatalf("create: %+v %v", d.Head, d.Warnings)
	}
	md, err := a.Local.DatasetWait(ctx, node.ScopeCluster, "pub", apiv0.DatasetWaitRequest{Target: d.Head.Target, Condition: apiv0.WaitMetadata, Acks: 2})
	if err != nil || !md.Satisfied {
		t.Fatalf("metadata: %+v %v", md, err)
	}
	var bd *apiv0.Dataset
	eventually(t, "member sees the head", func() bool {
		bd, err = b.Local.Dataset(ctx, node.ScopeCluster, "pub")
		return err == nil && bd.Head != nil && bd.Head.Rev == pub.Rev() && bd.Atrepo != nil && bd.Atrepo.Status == "active"
	})
	// The member does not fetch from the PDS and cannot replicate the repository yet: its
	// local state is syncing, not held, and the dataset is not ready at two copies.
	time.Sleep(3 * node.ClusterInterval)
	bd, _ = b.Local.Dataset(ctx, node.ScopeCluster, "pub")
	if l := bd.Content.Local; l == nil || l.Complete || bd.Content.State == cluster.ReadyReady {
		t.Fatalf("member state: %+v atrepo %+v", bd.Content, bd.Atrepo)
	}
	var repos int
	f.Set(func(f *atfix.Fixture) { repos = f.Requests["com.atproto.sync.getRepo"] })
	if repos != 1 {
		t.Fatalf("getRepo requests: %d (only the admin fetches)", repos)
	}

	// A since-diff update and a deactivation reach the member through the signed state.
	pub.Put("app.bsky.feed.post", "3bbb", map[string]any{"$type": "app.bsky.feed.post", "text": "second", "createdAt": "2026-01-01T00:00:00Z"})
	if d, err = a.Local.DatasetUpdate(ctx, node.ScopeCluster, "pub", apiv0.DatasetUpdateRequest{}); err != nil || d.Atrepo.Mode != "since" {
		t.Fatalf("update: %+v %v", d.Atrepo, err)
	}
	f.Set(func(f *atfix.Fixture) { f.Pub.Status = "takendown" })
	if d, err = a.Local.DatasetUpdate(ctx, node.ScopeCluster, "pub", apiv0.DatasetUpdateRequest{}); err != nil || d.Head.Status != cluster.AccountTakendown {
		t.Fatalf("takedown: %+v %v", d.Head, err)
	}
	eventually(t, "member sees the status", func() bool {
		bd, err = b.Local.Dataset(ctx, node.ScopeCluster, "pub")
		return err == nil && bd.Head.Rev == pub.Rev() && bd.Head.Status == cluster.AccountTakendown && bd.Atrepo.Redistribution == node.RedistPrivate
	})
	vs, err := a.Local.DatasetVersions(ctx, node.ScopeCluster, "pub")
	if err != nil || len(vs.Versions) != 2 || vs.Versions[1].Rev != pub.Rev() {
		t.Fatalf("versions: %+v %v", vs, err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(nutest.Timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
