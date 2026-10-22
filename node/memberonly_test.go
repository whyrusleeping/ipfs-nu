package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
)

// A public-mode cluster member refuses to mark a member-only dataset root public; other
// roots it holds may still be published (0069).
func TestPublicAddRefusesMemberOnlyDataset(t *testing.T) {
	ctx := context.Background()
	n := newLocal(t)
	if _, err := n.ClusterInit(ctx, apiv0.ClusterInitRequest{Name: "team", Mode: "public"}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.DatasetCreate(ctx, ScopeCluster, apiv0.DatasetCreateRequest{Name: "photos", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		r, err := add(t, n, apiv0.AddRequest{Path: p, Pin: true})
		if err != nil {
			t.Fatal(err)
		}
		return r.CID
	}
	member, public := write("a", "member-only bytes"), write("b", "public bytes")
	if _, err := n.DatasetUpdate(ctx, ScopeCluster, "photos", apiv0.DatasetUpdateRequest{Target: member}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.PublicAdd(ctx, apiv0.PublicRequest{CID: member}); !isCode(err, apiv0.CodeForbidden) || !strings.Contains(err.Error(), "members only") {
		t.Fatalf("public add of a member-only dataset head: %v", err)
	}
	if _, err := n.PublicAdd(ctx, apiv0.PublicRequest{CID: public}); err != nil {
		t.Fatalf("public add of other content: %v", err)
	}
}
