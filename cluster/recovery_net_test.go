package cluster_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/nutest"
)

// Clean-machine restore from surviving members (0044): the founding member (root and
// admin keys) of a two-member archive cluster loses its disk; its authority backup is
// restored onto an empty $NU_PATH with no content export, the restored node rejoins under
// the same identity, re-acquires every retained version from the surviving member and
// can still write.
func TestRestoreFromSurvivors(t *testing.T) {
	n := nutest.New(t, 2)
	a, b := n.Nodes[0], n.Nodes[1]
	storageCluster(t, a, b)
	if _, err := a.Local.DatasetCreate(ctx(t), node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "logs", Replicas: 2, Archive: true}); err != nil {
		t.Fatal(err)
	}
	var tids []string
	for range 2 {
		root := addDir(t, n, a, 3)
		d, err := a.Local.DatasetUpdate(ctx(t), node.ScopeCluster, "logs", apiv0.DatasetUpdateRequest{Target: root.String()})
		if err != nil || d.Version == nil {
			t.Fatalf("update %+v %v", d, err)
		}
		tids = append(tids, d.Version.TID)
	}
	complete := func(nd *nutest.Node) bool {
		vs := versionsOf(t, nd, "logs")
		return len(vs) == 2 && vs[tids[0]].Availability == apiv0.AvailComplete && vs[tids[1]].Availability == apiv0.AvailComplete
	}
	eventually(t, "both versions complete on b", func() bool { return complete(b) })

	// Back up a's authority offline, then wipe its machine.
	a.Stop()
	l, err := node.NewLocal(a.Repo)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "a.tar")
	f, _ := os.Create(backup)
	if _, err := l.BackupAuthority(f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	l.Close()
	if err := os.RemoveAll(a.Dir); err != nil {
		t.Fatal(err)
	}
	r, _, err := repo.Open(a.Dir)
	if err != nil {
		t.Fatal(err)
	}
	rf, _ := os.Open(backup)
	if _, err := node.RestoreAuthority(r, rf); err != nil {
		t.Fatal(err)
	}
	rf.Close()
	a.Repo = r
	a.Start()

	eventually(t, "a re-acquires both versions from b", func() bool { return complete(a) })
	if _, err := a.Local.Ls(ctx(t), "logs@"+tids[0]); err != nil {
		t.Fatalf("old version on the restored node: %v", err)
	}
	// The restored admin key still writes, and the survivor accepts it.
	root := addDir(t, n, a, 1)
	d, err := a.Local.DatasetUpdate(ctx(t), node.ScopeCluster, "logs", apiv0.DatasetUpdateRequest{Target: root.String()})
	if err != nil || d.Version == nil {
		t.Fatalf("write after restore: %+v %v", d, err)
	}
	eventually(t, "b sees the restored node's write", func() bool { return len(versionsOf(t, b, "logs")) == 3 })
}
