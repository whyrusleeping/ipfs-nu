package replica

import (
	"bytes"
	"slices"
	"testing"

	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
)

// A target linking a DRISL CID whose bytes are stored only as a raw blob (a "codec swap",
// 0077) is incomplete for the completeness check: the node is missing structure, the
// links its bytes would have as DRISL (the secret blob) are not among the target's
// blobs, and the same raw blob linked as raw in the same tree does not mask it. The
// memoized check answers the same.
func TestCheckCodecSwap(t *testing.T) {
	f := newFixture(t, nil)
	secret, err := f.st.AddBlob(bytes.NewReader([]byte("members only")), 12)
	if err != nil {
		t.Fatal(err)
	}
	evil, err := drisl.Marshal(tree.FileNode{Src: secret, Nu: tree.FileMeta{Size: 12}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.st.AddBlobHash(bytes.NewReader(evil), int64(len(evil)), cid.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	swapped := cid.New(cid.DRISL, raw.Multihash())
	b, err := drisl.Marshal(map[string]any{"$type": tree.TypeMASL, "resources": map[string]any{
		"a": map[string]any{"src": raw}, "b": map[string]any{"src": swapped}}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := cid.SumCID(cid.DRISL, cid.SHA256, b)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.Put(root, b); err != nil {
		t.Fatal(err)
	}
	d, _ := f.c.View().Dataset("ds")
	pl := cluster.NewPlacement(f.c.View(), d.Policy, f.self)
	for i := range 2 { // a fresh walk, then the memo
		chk := f.m.check(root, 0, pl, true, "nu-tree")
		if chk.err != nil || chk.complete() || chk.structMissing != 1 || !slices.Equal(chk.blobs, []cid.Multihash{raw.Multihash()}) {
			t.Fatalf("check %d: complete=%v structMissing=%d blobs=%v err=%v", i, chk.complete(), chk.structMissing, chk.blobs, chk.err)
		}
	}
	if _, _, known := f.m.Groups("ds", root); known {
		t.Fatal("Groups reported the structure of a codec-swapped target known")
	}
}
