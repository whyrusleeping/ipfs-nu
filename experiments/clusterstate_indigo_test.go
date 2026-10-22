package experiments

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	irepo "github.com/bluesky-social/indigo/atproto/repo"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// A real cluster state repository (written by the cluster package and exported as a
// `pull` answers it) loads in stock indigo: the P-256 did:key signature verifies, the
// MST verifies with Nu's data root, the rev is a TID, and the records read back (0069).
func TestIndigoVerifiesClusterStateCAR(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	k, _ := wire.GenerateNodeKey()
	c, err := cluster.Create(cluster.Options{Dir: filepath.Join(t.TempDir(), "c"), Store: st}, "lab", cluster.ModePrivate, k.Public().PeerID(), nil)
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := wire.GenerateNodeKey()
	w, err := c.Write(
		cluster.Op{Collection: cluster.NSMember, RKey: k2.Public().PeerID().String(), Value: &cluster.Member{Roles: []string{cluster.RoleStorage}, State: cluster.StateActive}},
		cluster.Op{Collection: cluster.NSDataset, RKey: "photos", Value: &cluster.Dataset{Source: cluster.Source{Local: true}, Policy: cluster.Policy{All: true}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	did := w.DID
	held, ok := c.Held(did)
	if !ok {
		t.Fatal("no held state repo")
	}
	var buf bytes.Buffer
	if err := c.WriteDiff(&buf, did, cid.CID{}, cid.CID{}); err != nil {
		t.Fatal(err)
	}
	commit, repo, err := irepo.LoadRepoFromCAR(ctx, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("indigo load: %v", err)
	}
	if commit.DID != did {
		t.Fatalf("commit did %s, want %s", commit.DID, did)
	}
	pub, err := atcrypto.ParsePublicDIDKey(did)
	if err != nil {
		t.Fatal(err)
	}
	if err := commit.VerifySignature(pub); err != nil {
		t.Fatalf("indigo signature check: %v", err)
	}
	if _, err := syntax.ParseTID(commit.Rev); err != nil {
		t.Fatalf("rev %q: %v", commit.Rev, err)
	}
	if err := repo.MST.Verify(); err != nil {
		t.Fatal(err)
	}
	root, err := repo.MST.RootCID()
	if err != nil || root.String() != commit.Data.String() {
		t.Fatalf("indigo root %v, commit data %s", root, commit.Data)
	}
	if commit.Rev != held.Rev {
		t.Fatalf("rev %s, held %s", commit.Rev, held.Rev)
	}
	for _, p := range []string{cluster.NSDataset + "/photos", cluster.NSMember + "/" + k2.Public().PeerID().String()} {
		nsid, rkey, err := syntax.ParseRepoPath(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		b, _, err := repo.GetRecordBytes(ctx, nsid, rkey)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if _, err := cluster.DecodeRecord(p, b); err != nil {
			t.Fatalf("%s does not decode back: %v", p, err)
		}
	}
}
