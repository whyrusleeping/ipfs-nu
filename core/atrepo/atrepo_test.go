package atrepo

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/mr-tron/base58"

	"github.com/whyrusleeping/nu/core/car"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
)

// atproto-interop-tests crypto vectors, then commit signing with both curves.
func TestCryptoAndCommits(t *testing.T) {
	var sigs []struct {
		MessageBase64, Algorithm, DIDDocSuite, PublicKeyDid, PublicKeyMultibase, SignatureBase64 string
		ValidSignature                                                                           bool
	}
	readJSON(t, "atproto-crypto/signature-fixtures.json", &sigs)
	for i, f := range sigs {
		key, err := ParseKey(f.PublicKeyDid)
		if err != nil {
			t.Fatal(err)
		}
		// Legacy DID-document suites carry bare compressed key bytes.
		raw, _ := base58.Decode(f.PublicKeyMultibase[1:])
		var legacy atcrypto.PublicKey
		if f.Algorithm == "ES256" {
			legacy, err = atcrypto.ParsePublicBytesP256(raw)
		} else {
			legacy, err = atcrypto.ParsePublicBytesK256(raw)
		}
		if err != nil || !legacy.Equal(key) {
			t.Fatalf("fixture %d: legacy key %v", i, err)
		}
		msg, _ := base64.RawStdEncoding.DecodeString(f.MessageBase64)
		sig, _ := base64.RawStdEncoding.DecodeString(f.SignatureBase64)
		if err := key.HashAndVerify(msg, sig); (err == nil) != f.ValidSignature {
			t.Errorf("fixture %d (%s): verify = %v, want valid=%v", i, f.Algorithm, err, f.ValidSignature)
		}
	}
	var k256 []struct{ PrivateKeyBytesHex, PublicDidKey string }
	readJSON(t, "atproto-crypto/w3c_didkey_K256.json", &k256)
	var p256 []struct{ PrivateKeyBytesBase58, PublicDidKey string }
	readJSON(t, "atproto-crypto/w3c_didkey_P256.json", &p256)
	var privs []atcrypto.PrivateKey
	for _, v := range k256 {
		b, _ := hex.DecodeString(v.PrivateKeyBytesHex)
		k, err := atcrypto.ParsePrivateBytesK256(b)
		checkDIDKey(t, k, err, v.PublicDidKey)
		privs = append(privs, k)
	}
	for _, v := range p256 {
		b, _ := base58.Decode(v.PrivateKeyBytesBase58)
		k, err := atcrypto.ParsePrivateBytesP256(b)
		checkDIDKey(t, k, err, v.PublicDidKey)
		privs = append(privs, k)
	}

	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, priv := range []atcrypto.PrivateKey{privs[0], privs[len(privs)-1]} {
		pub, _ := priv.PublicKey()
		key, err := ParseKey(pub.DIDKey())
		if err != nil {
			t.Fatal(err)
		}
		com := &Commit{DID: "did:plc:ewvi7nxzyoun6zhxrhs64oiz", Data: leaf, Rev: NextRev("", now)}
		if err := com.Sign(priv); err != nil {
			t.Fatal(err)
		}
		_, b, err := com.Encode()
		if err != nil {
			t.Fatal(err)
		}
		back, err := DecodeCommit(b)
		if err != nil || back.Verify(key) != nil {
			t.Fatalf("round trip: %v", err)
		}
		back.Rev = NextRev(back.Rev, now)
		if !errors.Is(back.Verify(key), ErrSignature) {
			t.Fatal("altered commit verified")
		}
		if err := com.CheckRev(now.Add(-4*time.Minute), 0); err != nil {
			t.Fatalf("rev within drift refused: %v", err)
		}
		if err := com.CheckRev(now.Add(-time.Hour), 0); !errors.Is(err, ErrFutureRev) {
			t.Fatalf("future rev accepted: %v", err)
		}
	}
}

func checkDIDKey(t *testing.T, k atcrypto.PrivateKey, err error, want string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := k.PublicKey()
	if err != nil || pub.DIDKey() != want {
		t.Fatalf("did:key %v, want %s (%v)", pub, want, err)
	}
}

// atproto-interop-tests firehose vectors: the slice carries every block the reference
// proof lists, and inversion over exactly the reference blocks reaches rootBeforeCommit.
func TestFirehoseVectors(t *testing.T) {
	var fx []struct {
		Comment, LeafValue, RootBeforeCommit, RootAfterCommit string
		Keys, Adds, Dels, BlocksInProof                       []string
	}
	readJSON(t, "atproto-firehose/commit-proof-fixtures.json", &fx)
	for _, f := range fx {
		val := cid.MustParse(f.LeafValue)
		st := mst.MemStore{}
		tr := LoadTree(st, cid.CID{})
		for _, k := range f.Keys {
			_ = tr.Put(k, val)
		}
		before := flush(t, tr)
		var ops []Op
		for _, k := range f.Adds {
			_ = tr.Put(k, val)
			ops = append(ops, Op{Path: k, CID: val})
		}
		for _, k := range f.Dels {
			_, _ = tr.Delete(k)
			ops = append(ops, Op{Path: k, Prev: val})
		}
		after := flush(t, tr)
		if before.String() != f.RootBeforeCommit || after.String() != f.RootAfterCommit {
			t.Errorf("%s: roots %s %s", f.Comment, before, after)
			continue
		}
		nodes, gotOps, err := sliceNodes(st, before, after)
		if err != nil {
			t.Fatalf("%s: %v", f.Comment, err)
		}
		if len(gotOps) != len(ops) {
			t.Errorf("%s: ops %v", f.Comment, gotOps)
		}
		ref := mst.MemStore{}
		for _, s := range f.BlocksInProof {
			c := cid.MustParse(s)
			if !slices.Contains(nodes, c) {
				t.Errorf("%s: slice lacks %s", f.Comment, s)
			}
			ref[c] = st[c]
		}
		if got, err := Invert(ref, after, ops); err != nil || got != before {
			t.Errorf("%s: inversion over the reference blocks: %s %v", f.Comment, got, err)
		}
		// An incomplete op list does not invert to the previous root.
		if got, _ := Invert(ref, after, ops[1:]); got == before {
			t.Errorf("%s: partial ops inverted to prevData", f.Comment)
		}
	}
}

type realRepo struct{ file, key string }

var realRepos = []realRepo{
	{"ts-pds-odfsmykcwlxgdaozogi7x4u2.car", "zQ3shMvSkxvkPvEMBj62C69RrVvxwDpeWa5gGUZ53nhJxvGhW"},
	{"ts-pds-rpozqnrysnmae4hegfralite.car", "zQ3shoQrpowBZqq8SftAybKcpjnrFcvLbhaCxvnzFHRXRDiqY"},
	{"cocoon-a3ucdy4frc42wqq4oqkwrp6k.car", "zQ3shZB4ipxkuhwFnG8TqaCLYTNtUPWPXSA5CQqYaMzSY8EbR"},
	{"cocoon-idigcrknb3idfulkp4moaxik.car", "zQ3sheV3vpzbdCAUssk8vG24eWfVxjQTS3fgC1tYpAJU7jphr"},
}

func readCARFile(t testing.TB, name string) ([]byte, mst.MemStore, cid.CID) {
	t.Helper()
	b, err := os.ReadFile(vecDir + "atproto-repos/" + name)
	if err != nil {
		t.Fatal(err)
	}
	st := mst.MemStore{}
	root, err := ReadCAR(bytes.NewReader(b), st, 0)
	if err != nil {
		t.Fatal(err)
	}
	return b, st, root
}

func carBlocks(t testing.TB, b []byte) []car.Block {
	t.Helper()
	cr, err := car.NewReader(bytes.NewReader(b), car.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var out []car.Block
	for {
		blk, _, err := cr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, blk)
	}
}

// Real getRepo CARs from two PDS implementations verify (signature against the DID
// document key, full tree and records) and round-trip: every exported block is the
// source's block byte for byte, the export holds all of the repo, and re-exporting the
// export reproduces it exactly. Source order is not streamable, so file bytes differ.
func TestRealRepos(t *testing.T) {
	for _, rr := range realRepos {
		src, st, root := readCARFile(t, rr.file)
		key, err := ParseKey(rr.key)
		if err != nil {
			t.Fatal(err)
		}
		r, stats, err := Verify(st, root, VerifyOptions{Key: key})
		if err != nil {
			t.Fatalf("%s: %v", rr.file, err)
		}
		wantNC := 0
		if rr.file == "cocoon-idigcrknb3idfulkp4moaxik.car" {
			wantNC = 1 // a profile record with a float
		}
		if len(stats.NonConforming) != wantNC {
			t.Errorf("%s: non-conforming %v", rr.file, stats.NonConforming)
		}
		var out bytes.Buffer
		if err := Export(&out, st, root); err != nil {
			t.Fatal(err)
		}
		orig := map[cid.CID][]byte{}
		for _, b := range carBlocks(t, src) {
			orig[b.CID] = b.Data
		}
		exp := carBlocks(t, out.Bytes())
		if exp[0].CID != root || exp[1].CID != r.Head.Data {
			t.Errorf("%s: export does not start with commit, root node", rr.file)
		}
		for _, b := range exp {
			if !bytes.Equal(orig[b.CID], b.Data) {
				t.Errorf("%s: block %s differs from the source", rr.file, b.CID)
			}
		}
		if len(exp) != stats.Records+stats.Nodes+1 && wantNC == 0 { // records may repeat
			t.Logf("%s: %d blocks for %d records, %d nodes", rr.file, len(exp), stats.Records, stats.Nodes)
		}
		st2 := mst.MemStore{}
		if _, err := ReadCAR(bytes.NewReader(out.Bytes()), st2, 0); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Verify(st2, root, VerifyOptions{Key: key}); err != nil {
			t.Fatalf("%s: export does not verify: %v", rr.file, err)
		}
		var again bytes.Buffer
		if err := Export(&again, st2, root); err != nil || !bytes.Equal(again.Bytes(), out.Bytes()) {
			t.Errorf("%s: re-export differs (%v)", rr.file, err)
		}
	}
}

// Nu's proof for a record (whose path crosses an empty intermediate node) carries exactly
// the blocks of the PDS's getRecord response, and both verify.
func TestProofMatchesGetRecord(t *testing.T) {
	const path = "app.bsky.feed.like/3lvwmxcdh2s27"
	_, st, root := readCARFile(t, "ts-pds-rpozqnrysnmae4hegfralite.car")
	key, _ := ParseKey(realRepos[1].key)
	o := VerifyOptions{Key: key, DID: "did:plc:rpozqnrysnmae4hegfralite"}

	theirs, err := os.ReadFile(vecDir + "atproto-repos/ts-pds-rpozqnrysnmae4hegfralite.getRecord-like-3lvwmxcdh2s27.car")
	if err != nil {
		t.Fatal(err)
	}
	ours, err := Prove(st, root, path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[cid.CID]bool{}
	for _, b := range carBlocks(t, theirs) {
		want[b.CID] = true
	}
	got := map[cid.CID]bool{}
	for _, b := range ours {
		got[b.CID] = true
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("proof blocks %v, getRecord %v", got, want)
	}
	var buf bytes.Buffer
	if err := WriteCAR(&buf, root, ours); err != nil {
		t.Fatal(err)
	}
	for _, carBytes := range [][]byte{theirs, buf.Bytes()} {
		p, err := VerifyRecord(bytes.NewReader(carBytes), path, o)
		if err != nil || !p.CID.Defined() || p.CommitCID != root {
			t.Fatalf("verify: %+v %v", p, err)
		}
	}
	// Absence proof.
	abs, _ := Prove(st, root, "app.bsky.feed.like/3lvwmxcdh2s28")
	buf.Reset()
	_ = WriteCAR(&buf, root, abs)
	if p, err := VerifyRecord(bytes.NewReader(buf.Bytes()), "app.bsky.feed.like/3lvwmxcdh2s28", o); err != nil || p.CID.Defined() {
		t.Fatalf("absence: %+v %v", p, err)
	}
	// A proof missing a path node does not verify.
	buf.Reset()
	_ = WriteCAR(&buf, root, append(slices.Clone(ours[:2]), ours[3:]...))
	if _, err := VerifyRecord(bytes.NewReader(buf.Bytes()), path, o); err == nil {
		t.Fatal("incomplete proof verified")
	}
}

func record(t testing.TB, text string) []byte {
	b, err := drisl.Marshal(map[string]any{"$type": "io.ipfs.nu.test", "text": text})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A P-256 did:key repository (cluster-state style): commit, change, CAR slice, firehose
// verification by inversion, and applying the slice as a since-diff to the held state.
func TestDiffsAndApply(t *testing.T) {
	priv, _ := atcrypto.GeneratePrivateKeyP256()
	pub, _ := priv.PublicKey()
	did := pub.DIDKey()
	st := mst.MemStore{}
	now := time.Now()
	r := New(st, did)
	for i := range 200 {
		if _, err := r.Put(fmt.Sprintf("io.ipfs.nu.cluster.member/m%03d", i), record(t, fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	c1, err := r.Commit(priv, now)
	if err != nil {
		t.Fatal(err)
	}
	prevData, rev1 := r.Head.Data, r.Head.Rev
	held := mst.MemStore{}
	for k, v := range st {
		held[k] = v
	}
	_, _ = r.Delete("io.ipfs.nu.cluster.member/m010")
	_, _ = r.Put("io.ipfs.nu.cluster.member/m020", record(t, "changed"))
	_, _ = r.Put("io.ipfs.nu.cluster.member/x", record(t, "new"))
	c2, err := r.Commit(priv, now)
	if err != nil || r.Head.Rev <= rev1 {
		t.Fatal(err, r.Head.Rev, rev1)
	}
	_ = c1

	blocks, ops, err := Slice(st, prevData, c2)
	if err != nil {
		t.Fatal(err)
	}
	var acts []string
	for _, op := range ops {
		acts = append(acts, op.Action())
	}
	if !slices.Equal(acts, []string{"delete", "update", "create"}) {
		t.Fatalf("ops %v", acts)
	}
	slice := mst.MemStore{}
	for _, b := range blocks {
		slice[b.CID] = b.Data
	}
	o := VerifyOptions{Key: pub, DID: did}
	if _, err := VerifyCommit(slice, c2, ops, prevData, o); err != nil {
		t.Fatalf("verify commit: %v", err)
	}
	bad := slices.Clone(ops)
	bad[0].Prev = leaf
	if _, err := VerifyCommit(slice, c2, bad, prevData, o); err == nil {
		t.Fatal("wrong delete prev accepted")
	}

	var buf bytes.Buffer
	_ = WriteCAR(&buf, c2, blocks)
	a, err := ApplySince(held, prevData, bytes.NewReader(buf.Bytes()), o)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(a.Ops) != 3 || a.CommitCID != c2 {
		t.Fatalf("applied %+v", a)
	}
	for _, b := range a.Blocks {
		held[b.CID] = b.Data
	}
	if _, s, err := Verify(held, c2, o); err != nil || s.Records != 200 {
		t.Fatalf("held repo after apply: %v %+v", err, s)
	}
	// A diff whose chain does not reach the held state fails with ErrNotFound.
	if _, err := ApplySince(mst.MemStore{}, prevData, bytes.NewReader(buf.Bytes()), o); !errors.Is(err, mst.ErrNotFound) {
		t.Fatalf("broken chain: %v", err)
	}
}

// Key-mined trees: keys chosen to share one layer pile into one node, which the profile
// refuses to produce and to load.
func TestKeyMinedLimit(t *testing.T) {
	var keys []string
	for i := 0; len(keys) <= MaxNodeEntries; i++ {
		if k := fmt.Sprintf("app.bsky.feed.post/%08d", i); MST.Layer(k) == 0 {
			keys = append(keys, k)
		}
	}
	st := mst.MemStore{}
	tr := LoadTree(st, cid.CID{})
	for _, k := range keys {
		_ = tr.Put(k, leaf)
	}
	if _, err := tr.Flush(); !errors.Is(err, mst.ErrLimit) {
		t.Fatalf("flush of a %d-entry node: %v", len(keys), err)
	}
	unlimited := *MST
	unlimited.MaxEntries = 0
	tr = unlimited.Load(st, cid.CID{})
	for _, k := range keys {
		_ = tr.Put(k, leaf)
	}
	root := flush(t, tr)
	if _, _, err := LoadTree(st, root).Get(keys[0]); !errors.Is(err, mst.ErrLimit) {
		t.Fatalf("load of a %d-entry node: %v", len(keys), err)
	}
}

func FuzzDecodeCommit(f *testing.F) {
	for _, rr := range realRepos {
		_, st, root := readCARFile(f, rr.file)
		f.Add(st[root])
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := DecodeCommit(b)
		if err != nil {
			return
		}
		_, enc, err := c.Encode()
		if err != nil || !bytes.Equal(enc, b) {
			t.Fatalf("accepted a commit that does not re-encode: %v", err)
		}
	})
}

func FuzzMSTNode(f *testing.F) {
	_, st, _ := readCARFile(f, realRepos[1].file)
	for _, b := range st {
		if _, ok := nodeLinks(b); ok {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		n, err := nodeCodec{}.Decode(b)
		if err != nil {
			return
		}
		enc, err := nodeCodec{}.Encode(n)
		if err != nil || !bytes.Equal(enc, b) {
			t.Fatalf("accepted a node that does not re-encode: %v", err)
		}
		c, _ := cid.SumCID(cid.DRISL, cid.SHA256, b)
		_ = LoadTree(mst.MemStore{c: b}, c).Walk("", func(string, cid.CID) (bool, error) { return true, nil })
	})
}

// CAR slices are untrusted: reading one and verifying or applying it must fail cleanly.
func FuzzCARSlice(f *testing.F) {
	priv, _ := atcrypto.ParsePrivateBytesP256(bytes.Repeat([]byte{7}, 32))
	st := mst.MemStore{}
	r := New(st, "did:plc:ewvi7nxzyoun6zhxrhs64oiz")
	for i := range 20 {
		_, _ = r.Put(fmt.Sprintf("io.ipfs.nu.test/k%d", i), record(f, fmt.Sprint(i)))
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, _ = r.Commit(priv, now)
	prev := r.Head.Data
	_, _ = r.Delete("io.ipfs.nu.test/k3")
	_, _ = r.Put("io.ipfs.nu.test/k30", record(f, "x"))
	c2, _ := r.Commit(priv, now)
	blocks, ops, err := Slice(st, prev, c2)
	if err != nil {
		f.Fatal(err)
	}
	var buf bytes.Buffer
	_ = WriteCAR(&buf, c2, blocks)
	f.Add(buf.Bytes())
	o := VerifyOptions{Now: now}
	f.Fuzz(func(t *testing.T, b []byte) {
		s := mst.MemStore{}
		root, err := ReadCAR(bytes.NewReader(b), s, 1000)
		if err != nil {
			return
		}
		_, _ = VerifyCommit(s, root, ops, prev, o)
		_, _ = ApplySince(st, prev, bytes.NewReader(b), o)
	})
}

// The atrepo walk profile reaches every block of a repo from its commit, does not follow
// record links, and follows blob refs only when blobs are selected.
func TestWalkProfile(t *testing.T) {
	_, st, root := readCARFile(t, realRepos[1].file)
	walk := func(st mst.MemStore, root cid.CID, links func(cid.CID, []byte) ([]cid.CID, error)) (seen map[cid.CID]bool, missing int) {
		seen = map[cid.CID]bool{}
		stack := []cid.CID{root}
		for len(stack) > 0 {
			c := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[c] {
				continue
			}
			seen[c] = true
			data, ok := st[c]
			if !ok {
				missing++
				continue
			}
			kids, err := links(c, data)
			if err != nil {
				t.Fatal(err)
			}
			stack = append(stack, kids...)
		}
		return seen, missing
	}
	var out bytes.Buffer
	_ = Export(&out, st, root)
	seen, missing := walk(st, root, Links)
	if missing != 0 || len(seen) != len(carBlocks(t, out.Bytes())) {
		t.Fatalf("walk saw %d blocks (%d missing)", len(seen), missing)
	}

	blob, _ := cid.SumCID(cid.Raw, cid.SHA256, []byte("image"))
	rec, _ := drisl.Marshal(map[string]any{
		"$type":   "app.bsky.feed.post",
		"subject": map[string]any{"cid": leaf, "uri": "at://did:plc:x/app.bsky.feed.post/1"},
		"embed":   map[string]any{"$type": "blob", "ref": blob, "mimeType": "image/png", "size": int64(5)},
	})
	st2 := mst.MemStore{}
	r := New(st2, "did:plc:ewvi7nxzyoun6zhxrhs64oiz")
	_, _ = r.Put("app.bsky.feed.post/3jqfcqzm3fo2j", rec)
	priv, _ := atcrypto.GeneratePrivateKeyP256()
	c, _ := r.Commit(priv, time.Now())
	if _, missing := walk(st2, c, Links); missing != 0 {
		t.Fatalf("atrepo walk followed record links (%d missing)", missing)
	}
	if seen, missing := walk(st2, c, LinksWithBlobs); missing != 1 || !seen[blob] {
		t.Fatalf("atrepo+blobs walk: %d missing", missing)
	}
}
