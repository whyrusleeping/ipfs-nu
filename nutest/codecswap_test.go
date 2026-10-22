package nutest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/nutest"
)

// addSHA256 adds dir recursively with SHA-256 blobs (core DASL) and returns the root.
func addSHA256(t *testing.T, nd *nutest.Node, dir string) cid.CID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), nutest.Timeout)
	defer cancel()
	op, err := nd.Local.Add(ctx, apiv0.AddRequest{Path: dir, Recursive: true, Pin: true, Hash: "sha256"})
	if err != nil {
		t.Fatal(err)
	}
	final, err := nd.Local.WaitOperation(ctx, op.ID, nutest.Timeout)
	if err != nil || final.State != apiv0.StateSucceeded {
		t.Fatalf("add: %+v %v", final, err)
	}
	var res apiv0.AddResult
	if err := json.Unmarshal(final.Result, &res); err != nil {
		t.Fatal(err)
	}
	return cid.MustParse(res.CID)
}

// A tree that links the same bytes as a raw blob and as a DRISL node (0079) completes
// on a fetching peer: the fetcher, the /nu/tree/1 walk on both sides and the layer-1
// walk treat them as two objects. Keyed by multihash, the node was skipped as already
// seen after the blob and the fetch never completed.
func TestFetchSameBytesRawAndDRISL(t *testing.T) {
	t.Run("tree", func(t *testing.T) {
		// A directory whose file "a" holds the bytes of its subdirectory's MST node: with
		// SHA-256 blobs, entry a's blob and entry sub's directory share a multihash.
		n := nutest.New(t, 2)
		a, b := n.Nodes[0], n.Nodes[1]
		src := t.TempDir()
		os.MkdirAll(filepath.Join(src, "sub"), 0o755)
		os.WriteFile(filepath.Join(src, "sub", "f.txt"), []byte("in sub"), 0o644)
		sub := addSHA256(t, a, filepath.Join(src, "sub"))
		hdr, err := a.Local.Store.Get(sub)
		if err != nil {
			t.Fatal(err)
		}
		h, err := tree.DecodeHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		node, err := a.Local.Store.Get(h.Root)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(src, "a"), node, 0o644)
		root := addSHA256(t, a, src)
		raw := cid.New(cid.Raw, h.Root.Multihash())
		if !a.Has(raw) || !a.Has(h.Root) {
			t.Fatal("fixture: the provider must hold the bytes as raw and as DRISL")
		}
		if err := b.Fetch(root, nil, a); err != nil {
			t.Fatal(err)
		}
		if !b.Has(root) || !b.Has(h.Root) || !b.Has(raw) {
			t.Fatalf("incomplete after fetch: root %v, dir %v, blob %v", b.Has(root), b.Has(h.Root), b.Has(raw))
		}
	})
	t.Run("layer-1", func(t *testing.T) {
		// A MASL bundle root (not a /nu/tree/1 root) linking the bytes both ways: only the
		// fetcher's level walk runs.
		n := nutest.New(t, 2)
		a, b := n.Nodes[0], n.Nodes[1]
		st := a.Local.Store
		secret, err := st.AddBlob(bytes.NewReader([]byte("linked")), 6)
		if err != nil {
			t.Fatal(err)
		}
		fileNode, err := drisl.Marshal(tree.FileNode{Src: secret, Nu: tree.FileMeta{Size: 6}})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := st.AddBlobHash(bytes.NewReader(fileNode), int64(len(fileNode)), cid.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		asNode := cid.New(cid.DRISL, raw.Multihash())
		if err := st.Put(asNode, fileNode); err != nil {
			t.Fatal(err)
		}
		bundle, err := drisl.Marshal(map[string]any{"$type": tree.TypeMASL, "resources": map[string]any{
			"a": map[string]any{"src": raw}, "b": map[string]any{"src": asNode}}})
		if err != nil {
			t.Fatal(err)
		}
		root, err := cid.SumCID(cid.DRISL, cid.SHA256, bundle)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Put(root, bundle); err != nil {
			t.Fatal(err)
		}
		if !a.Has(root) {
			t.Fatal("fixture incomplete on the provider")
		}
		if err := b.Fetch(root, nil, a); err != nil {
			t.Fatal(err)
		}
		if !b.Has(root) || !b.Has(asNode) || !b.Has(secret) {
			t.Fatalf("incomplete after fetch: root %v, node %v, its blob %v", b.Has(root), b.Has(asNode), b.Has(secret))
		}
	})
}
