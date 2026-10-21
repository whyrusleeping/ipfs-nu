package cluster

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time { return f.t }

func testStore(t *testing.T) *store.Store {
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func peerID(t *testing.T) wire.PeerID {
	k, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	return k.Public().PeerID()
}

// founder creates a private cluster on a fresh store.
func founder(t *testing.T, clk *fakeClock) *Cluster {
	c, err := Create(Options{Dir: filepath.Join(t.TempDir(), "c"), Store: testStore(t), Now: clk.now}, "lab", ModePrivate, peerID(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// joiner prepares a second node of c's cluster from a pre-authorized token.
func joiner(t *testing.T, c *Cluster, clk *fakeClock) *Cluster {
	tok := &Token{Genesis: c.Genesis(), Boot: []Boot{{Peer: peerID(t)}}, Expires: clk.t.Add(time.Hour), Peer: peerID(t)}
	s, err := tok.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if tok, err = ParseToken(s); err != nil {
		t.Fatal(err)
	}
	j, err := JoinToken(Options{Dir: filepath.Join(t.TempDir(), "c"), Store: testStore(t), Now: clk.now}, tok)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// pull replicates from → to the way the protocol does: heads, wants, diff, apply.
func pull(t *testing.T, from, to *Cluster) {
	t.Helper()
	for range 4 { // authorization can unlock more repositories
		wants := to.Wants(from.Heads())
		if len(wants) == 0 {
			return
		}
		for did, at := range wants {
			var buf bytes.Buffer
			if err := from.WriteDiff(&buf, did, to.SinceFor(did), at); err != nil {
				t.Fatalf("diff %s: %v", did, err)
			}
			if _, err := to.Apply(did, at, &buf); err != nil {
				t.Fatalf("apply %s: %v", did, err)
			}
		}
	}
}

func newKey(t *testing.T, c *Cluster) string {
	k, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.saveKey(k); err != nil {
		t.Fatal(err)
	}
	pub, _ := k.PublicKey()
	return pub.DIDKey()
}

// Writers are confined to the heads of datasets that list them; admins' concurrent
// writes resolve by (hlc, did) everywhere; root key records beat admin ones.
func TestAuthorizationAndLWW(t *testing.T) {
	clk := &fakeClock{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	a := founder(t, clk)
	b := joiner(t, a, clk)
	adminB := newKey(t, b)
	writer := newKey(t, b)
	root := a.Genesis().RootKey
	if _, err := a.WriteAs(root,
		Op{Collection: NSKey, RKey: adminB, Value: &Key{Role: RoleAdmin}},
		Op{Collection: NSKey, RKey: writer, Value: &Key{Role: RoleWriter}},
		Op{Collection: NSDataset, RKey: "photos", Value: &Dataset{Source: Source{Local: true}, Policy: Policy{Replicas: 2}, Writers: []string{writer}}},
	); err != nil {
		t.Fatal(err)
	}
	pull(t, a, b)
	if !b.HoldsKey(RoleAdmin) {
		t.Fatal("b does not see its admin grant")
	}
	target := "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"
	if _, err := b.WriteAs(writer,
		Op{Collection: NSHead, RKey: "photos", Value: &Head{Target: target, Kind: "file"}},
		Op{Collection: NSHead, RKey: "other", Value: &Head{Target: target, Kind: "file"}},
		Op{Collection: NSMember, RKey: peerID(t).String(), Value: &Member{Roles: []string{RoleStorage}, State: StateActive}},
	); err != nil {
		t.Fatal(err)
	}
	// Concurrent dataset writes by two admins at the same wall time.
	if _, err := a.Write(Op{Collection: NSDataset, RKey: "logs", Value: &Dataset{Source: Source{Local: true}, Note: "a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(Op{Collection: NSDataset, RKey: "logs", Value: &Dataset{Source: Source{Local: true}, Note: "b"}}); err != nil {
		t.Fatal(err)
	}
	// An admin cannot override the root on a key record.
	if _, err := b.Write(Op{Collection: NSKey, RKey: writer, Delete: true}); err != nil {
		t.Fatal(err)
	}
	pull(t, a, b)
	pull(t, b, a)
	va, vb := a.View(), b.View()
	for _, v := range []*View{va, vb} {
		if h, ok := v.Head("photos"); !ok || h.Value.Target != target || h.Signer != writer {
			t.Fatalf("photos head = %+v, %v", h, ok)
		}
		if _, ok := v.Head("other"); ok {
			t.Fatal("out-of-scope head accepted")
		}
		if len(v.Members) != 1 {
			t.Fatalf("writer-written member accepted: %d members", len(v.Members))
		}
		if v.Ignored < 2 {
			t.Fatalf("ignored = %d, want >= 2", v.Ignored)
		}
		if k := v.Keys[writer]; k == nil || k.Role != RoleWriter {
			t.Fatal("admin tombstone overrode the root's key record")
		}
	}
	la, _ := va.Dataset("logs")
	lb, _ := vb.Dataset("logs")
	if la.Note != lb.Note {
		t.Fatalf("LWW diverged: %q vs %q", la.Note, lb.Note)
	}
	if va.Datasets["logs"].Signer != vb.Datasets["logs"].Signer {
		t.Fatal("winner differs")
	}
}

// A revoked admin's later commits are refused and its state freezes at the named commit,
// also for a node that already holds a later commit.
func TestRevocation(t *testing.T) {
	clk := &fakeClock{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	a := founder(t, clk)
	b := joiner(t, a, clk)
	c := joiner(t, a, clk)
	adminB := newKey(t, b)
	root := a.Genesis().RootKey
	if _, err := a.WriteAs(root, Op{Collection: NSKey, RKey: adminB, Value: &Key{Role: RoleAdmin}}); err != nil {
		t.Fatal(err)
	}
	pull(t, a, b)
	w1, err := b.Write(Op{Collection: NSDataset, RKey: "kept", Value: &Dataset{Source: Source{Local: true}}})
	if err != nil {
		t.Fatal(err)
	}
	pull(t, b, a)
	clk.t = clk.t.Add(time.Second)
	if _, err := b.Write(Op{Collection: NSDataset, RKey: "late", Value: &Dataset{Source: Source{Local: true}}}); err != nil {
		t.Fatal(err)
	}
	pull(t, a, c)
	pull(t, b, c) // c holds b's later commit
	if _, ok := c.View().Dataset("late"); !ok {
		t.Fatal("c should see late before the revocation")
	}
	if _, err := a.WriteAs(root, Op{Collection: NSKey, RKey: adminB, Value: &Key{Role: RoleAdmin, Revoked: &Revocation{After: w1.Commit, Rev: w1.Rev}}}); err != nil {
		t.Fatal(err)
	}
	pull(t, b, a) // refused: nothing newer than the cutoff is wanted
	pull(t, a, c) // c learns the revocation and fetches the cutoff commit from a
	for name, v := range map[string]*View{"a": a.View(), "c": c.View()} {
		if _, ok := v.Dataset("kept"); !ok {
			t.Fatalf("%s lost the record accepted before the revocation", name)
		}
		if _, ok := v.Dataset("late"); ok {
			t.Fatalf("%s kept a record from after the revocation", name)
		}
	}
	if _, err := b.Write(Op{Collection: NSDataset, RKey: "x", Value: &Dataset{Source: Source{Local: true}}}); err != nil {
		t.Fatal(err) // b does not know yet
	}
	var buf bytes.Buffer
	h, _ := b.Held(adminB)
	_ = b.WriteDiff(&buf, adminB, cid.CID{}, h.Commit)
	if _, err := a.Apply(adminB, cid.CID{}, &buf); err == nil {
		t.Fatal("a accepted a commit after the revocation")
	}
}
