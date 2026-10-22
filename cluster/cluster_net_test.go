package cluster_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/client"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/nutest"
	"github.com/whyrusleeping/nu/proto"
)

func init() { node.ClusterInterval = 100 * time.Millisecond }

// slow bounds operations and waits in these tests: generous, because they share a busy
// machine; a real failure still fails, only later.
const slow = 45 * time.Second

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), slow)
	t.Cleanup(cancel)
	return c
}

// eventually polls cond until it holds or slow expires.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(slow)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// form creates a cluster in mode on n0 and joins n1 with a pre-authorized invite and n2
// with a secret one.
func form(t *testing.T, n *nutest.Network, mode string) {
	t.Helper()
	a, b, c := n.Nodes[0], n.Nodes[1], n.Nodes[2]
	if _, err := a.Local.ClusterInit(ctx(t), apiv0.ClusterInitRequest{Name: "lab", Mode: mode}); err != nil {
		t.Fatal(err)
	}
	pre, err := a.Local.ClusterInvite(ctx(t), apiv0.ClusterInviteRequest{Peer: b.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if !pre.Metadata.Acknowledged {
		t.Fatalf("invite metadata not acknowledged: %+v", pre.Metadata)
	}
	if _, err := b.Local.ClusterJoin(ctx(t), apiv0.ClusterJoinRequest{Token: pre.Token}); err != nil {
		t.Fatalf("pre-authorized join: %v", err)
	}
	sec, err := a.Local.ClusterInvite(ctx(t), apiv0.ClusterInviteRequest{Role: cluster.RoleClient})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Local.ClusterJoin(ctx(t), apiv0.ClusterJoinRequest{Token: sec.Token}); err != nil {
		t.Fatalf("secret join: %v", err)
	}
	// The secret is one-time.
	if _, ok := a.Local.NetCluster().View().Invites[sec.InviteID]; !ok || !a.Local.NetCluster().View().Invites[sec.InviteID].Deleted {
		t.Fatal("used invite not tombstoned")
	}
	converged(t, n, "membership", func(v *cluster.View) bool { return len(v.MemberIDs()) == 3 }, a, b, c)
}

func converged(t *testing.T, n *nutest.Network, what string, cond func(*cluster.View) bool, nodes ...*nutest.Node) {
	t.Helper()
	eventually(t, what, func() bool {
		for _, nd := range nodes {
			if !nd.Running() || !cond(nd.Local.NetCluster().View()) {
				return false
			}
		}
		return true
	})
}

// Three private-mode members converge on signed state, refuse out-of-scope records,
// resolve a concurrent write by LWW, keep non-members out, and apply a revocation
// written while partitioned.
func TestPrivateCluster(t *testing.T) {
	n := nutest.New(t, 4)
	a, b, c, outsider := n.Nodes[0], n.Nodes[1], n.Nodes[2], n.Nodes[3]
	form(t, n, cluster.ModePrivate)

	// Dataset create on the admin node: metadata acknowledged, content reported apart.
	ds, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "photos"})
	if err != nil {
		t.Fatal(err)
	}
	if !ds.Metadata.Acknowledged || len(ds.Metadata.AckedBy) == 0 || ds.Content.State != "none" {
		t.Fatalf("dataset result %+v", ds)
	}
	if _, err := b.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "nope"}); err == nil {
		t.Fatal("a member without an admin key created a dataset")
	}

	// A writer key on b, listed for "photos" only.
	bc := b.Local.NetCluster()
	writer, err := bc.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	ac := a.Local.NetCluster()
	if _, err := ac.Write(
		cluster.Op{Collection: cluster.NSKey, RKey: writer, Value: &cluster.Key{Role: cluster.RoleWriter}},
		cluster.Op{Collection: cluster.NSDataset, RKey: "photos", Value: &cluster.Dataset{Source: cluster.Source{Local: true}, Policy: cluster.Policy{Replicas: 2}, Writers: []string{writer}}},
	); err != nil {
		t.Fatal(err)
	}
	converged(t, n, "writer grant", func(v *cluster.View) bool { return v.Keys[writer] != nil }, b)
	target := "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"
	if _, err := bc.WriteAs(writer,
		cluster.Op{Collection: cluster.NSHead, RKey: "photos", Value: &cluster.Head{Target: target, Kind: "file"}},
		cluster.Op{Collection: cluster.NSHead, RKey: "other", Value: &cluster.Head{Target: target, Kind: "file"}},
		cluster.Op{Collection: cluster.NSMember, RKey: outsider.ID.String(), Value: &cluster.Member{Roles: []string{cluster.RoleStorage}, State: cluster.StateActive}},
	); err != nil {
		t.Fatal(err)
	}
	converged(t, n, "writer head", func(v *cluster.View) bool {
		h, ok := v.Head("photos")
		_, other := v.Head("other")
		return ok && h.Signer == writer && !other && !v.IsMember(outsider.ID) && v.Ignored >= 2
	}, a, b, c)
	if d, err := c.Local.Dataset(ctx(t), node.ScopeCluster, "photos"); err != nil || d.Head == nil || d.Content.State == cluster.ReadyReady || d.Content.Target != target || len(d.Heads) != 1 {
		t.Fatalf("dataset status on c: %+v, %v", d, err)
	}

	// Concurrent LWW: a second admin on b writes the same record while partitioned.
	admin, err := bc.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Write(cluster.Op{Collection: cluster.NSKey, RKey: admin, Value: &cluster.Key{Role: cluster.RoleAdmin}}); err != nil {
		t.Fatal(err)
	}
	converged(t, n, "admin grant", func(v *cluster.View) bool { return v.Keys[admin] != nil }, b)
	n.Partition(a, b)
	n.Partition(a, c)
	if _, err := ac.Write(cluster.Op{Collection: cluster.NSDataset, RKey: "logs", Value: &cluster.Dataset{Source: cluster.Source{Local: true}, Note: "from a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := bc.Write(cluster.Op{Collection: cluster.NSDataset, RKey: "logs", Value: &cluster.Dataset{Source: cluster.Source{Local: true}, Note: "from b"}}); err != nil {
		t.Fatal(err)
	}
	// Revocation while partitioned: a removes b; c learns it only after healing.
	if _, err := a.Local.ClusterMemberRemove(ctx(t), apiv0.ClusterMemberRemoveRequest{Peer: b.ID.String(), Force: true}); err != nil {
		t.Fatal(err)
	}
	if !c.Local.NetCluster().View().IsMember(b.ID) {
		t.Fatal("c learned the removal through a partition")
	}
	n.Heal(a, b)
	n.Heal(a, c)
	eventually(t, "LWW and removal", func() bool {
		var notes []string
		for _, nd := range []*nutest.Node{a, c} {
			v := nd.Local.NetCluster().View()
			d, ok := v.Dataset("logs")
			if !ok || v.IsMember(b.ID) {
				return false
			}
			notes = append(notes, d.Note)
		}
		return notes[0] == notes[1]
	})
	eventually(t, "b disconnected", func() bool { return !a.ConnectedTo(b.ID) && !c.ConnectedTo(b.ID) })

	// Non-members (the outsider, the removed b) cannot open streams or even connect.
	if err := outsider.Swarm.Connect(ctx(t), a.ID, a.Addrs()); err == nil {
		t.Fatal("a non-member connected to a private member")
	}
	if err := b.Swarm.Connect(ctx(t), a.ID, a.Addrs()); err == nil {
		t.Fatal("a removed member connected")
	}
	// While a secret invite is live, a non-member gets only the join exchange.
	if _, err := a.Local.ClusterInvite(ctx(t), apiv0.ClusterInviteRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := outsider.Swarm.Connect(ctx(t), a.ID, a.Addrs()); err != nil {
		t.Fatalf("joiner connection refused while an invite is live: %v", err)
	}
	for _, p := range []string{proto.BytesID, proto.TreeID, proto.HaveID} {
		if s, err := outsider.Swarm.Host().OpenStream(ctx(t), a.ID, p); err == nil {
			s.Close()
			t.Fatalf("non-member opened %s", p)
		}
	}
	err = cluster.HeadsCall(ctx(t), outsider.Swarm.Host(), a.ID, ac.ID)
	var se *cluster.StatusError
	if !errors.As(err, &se) || se.Status != cluster.StatusForbidden {
		t.Fatalf("non-member heads exchange: %v", err)
	}
}

// A member that crashes converges after restart, from its persisted state.
func TestClusterRestart(t *testing.T) {
	n := nutest.New(t, 3)
	a, c := n.Nodes[0], n.Nodes[2]
	form(t, n, cluster.ModePrivate)
	c.Kill()
	for _, name := range []string{"one", "two"} {
		if _, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	c.Start()
	converged(t, n, "restart convergence", func(v *cluster.View) bool {
		_, one := v.Dataset("one")
		_, two := v.Dataset("two")
		return one && two && len(v.MemberIDs()) == 3
	}, n.Nodes...)
	eventually(t, "heartbeats", func() bool {
		hb := a.Local.ClusterService().Peers()[c.ID]
		return hb.Heartbeat != nil
	})
}

// Through the HTTP API (as the CLI uses it): init, secret invite, join, and a dataset
// whose metadata acknowledgment is reported apart from its content status.
func TestClusterAPI(t *testing.T) {
	n := nutest.New(t, 2, nutest.Options{API: true})
	a := client.New(n.Nodes[0].Daemon.APIAddr())
	b := client.New(n.Nodes[1].Daemon.APIAddr())
	if _, err := a.ClusterInit(ctx(t), apiv0.ClusterInitRequest{Name: "lab"}); err != nil {
		t.Fatal(err)
	}
	inv, err := a.ClusterInvite(ctx(t), apiv0.ClusterInviteRequest{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := b.ClusterJoin(ctx(t), apiv0.ClusterJoinRequest{Token: inv.Token})
	if err != nil {
		t.Fatal(err)
	}
	if !info.Member || len(info.Members) != 2 || info.Mode != cluster.ModePrivate {
		t.Fatalf("joined info %+v", info)
	}
	d, err := a.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "photos"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Metadata.Acknowledged || d.Content.State != "none" {
		t.Fatalf("dataset %+v", d)
	}
	if _, err := a.DatasetCreate(ctx(t), node.ScopeLocal, apiv0.DatasetCreateRequest{Name: "photos"}); err == nil {
		t.Fatal("a local dataset reused a networked dataset's name")
	}
	eventually(t, "dataset replicated", func() bool {
		_, err := b.Dataset(ctx(t), node.ScopeCluster, "photos")
		return err == nil
	})
}
