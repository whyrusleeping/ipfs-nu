package cluster_test

import (
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/nutest"
)

func versionsOf(t *testing.T, nd *nutest.Node, ds string) map[string]apiv0.VersionInfo {
	t.Helper()
	vs, err := nd.Local.DatasetVersions(ctx(t), node.ScopeCluster, ds)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]apiv0.VersionInfo{}
	for _, v := range vs.Versions {
		out[v.TID] = v
	}
	return out
}

// An archive on a two-member cluster uses ordinary placement (0047, E8): members fetch
// their owned blobs of every live version and report the live set; archive-wide
// readiness counts confirmed copies per blob over the live set (the writer's full copy
// covers k=1); a new version that cannot complete
// makes the archive not ready without changing the established availability of the
// older complete version; rollback's head write and its readiness are separate results.
func TestClusterArchive(t *testing.T) {
	n := nutest.New(t, 2)
	a, b := n.Nodes[0], n.Nodes[1]
	storageCluster(t, a, b)
	if _, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "logs", Replicas: 1, Archive: true}); err != nil {
		t.Fatal(err)
	}
	v1root := addDir(t, n, a, 6)
	d, err := a.Local.DatasetUpdate(ctx(t), node.ScopeCluster, "logs", apiv0.DatasetUpdateRequest{Target: v1root.String()})
	if err != nil || d.Version == nil {
		t.Fatalf("update %+v %v", d, err)
	}
	v1 := d.Version.TID
	w, err := b.Local.DatasetWait(ctx(t), node.ScopeCluster, "logs", apiv0.DatasetWaitRequest{Target: v1root.String(), Condition: apiv0.WaitArchive, Timeout: "40s"})
	if err != nil || !w.Satisfied || w.Required != 1 || len(w.Confirmed) == 0 || w.Live == "" || w.Copies == nil || w.Copies.Below != 0 {
		t.Fatalf("archive-ready wait: %+v %v", w, err)
	}
	eventually(t, "v1 established on b", func() bool {
		return versionsOf(t, b, "logs")[v1].Availability == apiv0.AvailComplete
	})

	// v2's content exists nowhere: the archive is not ready, v1 stays retained-complete.
	missing := "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"
	d2, err := a.Local.DatasetUpdate(ctx(t), node.ScopeCluster, "logs", apiv0.DatasetUpdateRequest{Target: missing, Kind: "blob"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b sees v2", func() bool { return len(versionsOf(t, b, "logs")) == 2 })
	vs, err := b.Local.DatasetVersions(ctx(t), node.ScopeCluster, "logs")
	if err != nil {
		t.Fatal(err)
	}
	if vs.Archive == nil || vs.Archive.State == cluster.ReadyReady {
		t.Fatalf("archive with an incomplete version: %+v", vs.Archive)
	}
	got := map[string]string{}
	for _, v := range vs.Versions {
		got[v.TID] = v.Availability
	}
	if got[v1] != apiv0.AvailComplete || got[d2.Version.TID] == apiv0.AvailComplete {
		t.Fatalf("availability: %v", got)
	}
	// Reading the incomplete version fails honestly; v1 reads through placement.
	if _, err := b.Local.Cat(ctx(t), "logs@"+d2.Version.TID); waitCode(err) != apiv0.CodeUnavailable {
		t.Fatalf("incomplete version read: %v", err)
	}
	if l, err := b.Local.Ls(ctx(t), "logs@"+v1); err != nil || len(l.Entries) != 6 {
		t.Fatalf("v1 listing on b: %+v %v", l, err)
	}
	rb, err := a.Local.DatasetRollback(ctx(t), node.ScopeCluster, "logs", apiv0.RollbackRequest{Version: v1})
	if err != nil || rb.Head.Target != v1root.String() || !rb.Metadata.Acknowledged {
		t.Fatalf("rollback %+v %v", rb, err)
	}
	if _, err := b.Local.DatasetPrune(ctx(t), node.ScopeCluster, "logs", apiv0.PruneRequest{}); waitCode(err) != apiv0.CodeNotSupported {
		t.Fatalf("networked prune must be dry-run only: %v", err)
	}
}
