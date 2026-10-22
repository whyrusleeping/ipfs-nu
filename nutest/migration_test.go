package nutest_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/bridge/ipfsfix"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/nutest"
)

// Cutover gates (docs/migration.md): an import into an archive dataset writes a version
// whose signed `imported` field references the provenance manifest; the cutover check then
// retrieves the head with a fresh client over the network and waits for the exact policy
// (two copies) before recommending cutover. A head written without an import, or a policy
// that is not yet met, is explained instead.
func TestImportCutover(t *testing.T) {
	old := node.ClusterInterval
	node.ClusterInterval = 200 * time.Millisecond
	defer func() { node.ClusterInterval = old }()
	n := nutest.New(t, 2)
	a, b := n.Nodes[0], n.Nodes[1]
	ctx := context.Background()
	if _, err := a.Local.ClusterInit(ctx, apiv0.ClusterInitRequest{Name: "mig", Mode: "public"}); err != nil {
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
	if _, err := a.Local.DatasetCreate(ctx, node.ScopeCluster, apiv0.DatasetCreateRequest{Name: "web", Replicas: 2, Archive: true}); err != nil {
		t.Fatal(err)
	}

	// A head that did not come from an import is not a cutover candidate; b is partitioned
	// away, so the policy cannot be met either.
	plain := a.AddBytes(n.RandBytes(4 << 10))
	if _, err := a.Local.DatasetUpdate(ctx, node.ScopeCluster, "web", apiv0.DatasetUpdateRequest{Target: plain.String()}); err != nil {
		t.Fatal(err)
	}
	n.Partition(a, b)
	res, err := a.Local.ImportCutover(ctx, apiv0.ImportCutoverRequest{Scope: node.ScopeCluster, Dataset: "web", Timeout: "500ms"})
	if err != nil || res.Recommend || res.Imported != nil || len(res.Missing) != 2 || res.Readiness.Satisfied {
		t.Fatalf("plain head: %v %+v", err, res)
	}
	n.Heal(a, b)

	d := ipfsfix.New()
	root := d.Files(map[string][]byte{"index.html": n.RandBytes(3 << 10), "big.bin": n.RandBytes(40 << 10)}, 16<<10)
	car := filepath.Join(t.TempDir(), "root.car")
	if err := os.WriteFile(car, d.CAR(root), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := a.Local.ImportIPFS(ctx, apiv0.IPFSImportRequest{Root: root.String(), CAR: car, Dataset: "web", Scope: node.ScopeCluster})
	if err != nil {
		t.Fatal(err)
	}
	final, err := a.Local.WaitOperation(ctx, o.ID, nutest.Timeout)
	if err != nil || final.State != apiv0.StateSucceeded {
		t.Fatalf("import: %v %+v", err, final)
	}
	var imp apiv0.IPFSImportResult
	if err := json.Unmarshal(final.Result, &imp); err != nil {
		t.Fatal(err)
	}
	if imp.Dataset == nil || imp.Dataset.Version == nil || imp.Dataset.Version.Imported == nil || imp.Dataset.Version.Imported.Manifest != imp.Manifest ||
		imp.Dataset.Version.Imported.Source != "ipfs://"+root.String() || imp.Dataset.Version.Root != imp.Root {
		t.Fatalf("dataset version lacks the imported reference: %+v", imp.Dataset)
	}
	res, err = a.Local.ImportCutover(ctx, apiv0.ImportCutoverRequest{Scope: node.ScopeCluster, Dataset: "web", Timeout: "20s"})
	if err != nil || !res.Recommend || res.Target != imp.Root || res.Imported == nil {
		t.Fatalf("cutover: %v %+v", err, res)
	}
	if !res.Retrieval.OK || res.Retrieval.Client == a.ID.String() || res.Retrieval.Files != 2 || res.Retrieval.Bytes != 43<<10 {
		t.Fatalf("fresh retrieval: %+v", res.Retrieval)
	}
	if !res.Readiness.Satisfied || res.Readiness.Required != 2 || len(res.Readiness.Confirmed) != 2 {
		t.Fatalf("readiness: %+v", res.Readiness)
	}
}
