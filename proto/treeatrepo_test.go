package proto

import (
	"bufio"
	"bytes"
	"fmt"
	"testing"

	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

func atFixture(t testing.TB) (*store.Store, *mst.Tree[cid.CID], cid.CID) {
	t.Helper()
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tr := atrepo.LoadTree(st, cid.CID{})
	for i := range 128 {
		c, b, err := drisl.Sum(map[string]any{"$type": "app.bsky.feed.post", "text": fmt.Sprintf("record %03d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Put(c, b); err != nil {
			t.Fatal(err)
		}
		if err := tr.Put(fmt.Sprintf("app.bsky.feed.post/%03d", i), c); err != nil {
			t.Fatal(err)
		}
	}
	return st, tr, atCommit(t, st, tr, "3m2z4n6q2ls22")
}
func atCommit(t testing.TB, st *store.Store, tr *mst.Tree[cid.CID], rev string) cid.CID {
	t.Helper()
	root, err := tr.Flush()
	if err != nil {
		t.Fatal(err)
	}
	com := atrepo.Commit{DID: "did:plc:nupublishertestaccount22", Data: root, Rev: rev, Sig: make([]byte, 64)}
	c, b, err := com.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(c, b); err != nil {
		t.Fatal(err)
	}
	return c
}
func atBody(t testing.TB, st *store.Store, req TreeRequest) []byte {
	t.Helper()
	b, err := req.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	if f, err := NewServer(st, ServerConfig{}).serveTree(b, w); f != nil || err != nil {
		t.Fatal(f, err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(out.Bytes())
	if _, err := wire.ReadMessage(r, MaxBytesHeaderSize); err != nil {
		t.Fatal(err)
	}
	return bytes.Clone(out.Bytes()[out.Len()-r.Len():])
}
func atSync(t *testing.T, src *store.Store, dst mst.MemStore, req TreeRequest) TreeResult {
	t.Helper()
	var total TreeResult
	for i := 0; i < 1000; i++ {
		body := atBody(t, src, req)
		res, err := readTree(bufio.NewReader(bytes.NewReader(body)), dst, &req)
		if err != nil {
			t.Fatalf("page %d cursor %+v: %v", i, req.Cursor, err)
		}
		total.Nodes += res.Nodes
		total.Bytes += res.Bytes
		total.Have = append(total.Have, res.Have...)
		if !res.More {
			return total
		}
		req.Cursor = res.Cursor
	}
	t.Fatal("unbounded pagination")
	return total
}

func TestAtrepoTreeDiffAndCursors(t *testing.T) {
	src, tr, old := atFixture(t)
	dst := mst.MemStore{}
	req := TreeRequest{Root: old, Profile: ProfileAtrepo, Scope: ScopeSubtree, Limit: 1}
	full := atSync(t, src, dst, req)
	empty := 0
	for _, b := range dst {
		if n, err := atrepo.MST.Codec.Decode(b); err == nil && len(n.Entries) == 0 && n.Left.Defined() {
			empty++
		}
	}
	if empty == 0 {
		t.Fatal("fixture did not exercise layered empty intermediate cursors")
	}
	c, b, _ := drisl.Sum(map[string]any{"$type": "app.bsky.feed.post", "text": "changed"})
	if err := src.Put(c, b); err != nil {
		t.Fatal(err)
	}
	if err := tr.Put("app.bsky.feed.post/064", c); err != nil {
		t.Fatal(err)
	}
	head := atCommit(t, src, tr, "3m2z4n6q2ls23")
	// Exact expected payload: reachable blocks not held in the old snapshot.
	var wantNodes int
	var wantBytes int64
	seen := map[cid.CID]bool{}
	var walk func(cid.CID)
	walk = func(c cid.CID) {
		if seen[c] {
			return
		}
		seen[c] = true
		b, err := src.Get(c)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := dst[c]; !ok {
			wantNodes++
			wantBytes += int64(len(b))
		}
		ls, err := atrepo.Links(c, b)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range ls {
			walk(l)
		}
	}
	walk(head)
	paged := mst.MemStore{}
	for c, b := range dst {
		paged[c] = b
	}
	req.Root, req.Have, req.Limit = head, []cid.CID{old}, 0
	diff := atSync(t, src, dst, req)
	pageReq := req
	pageReq.Limit = 1
	pageDiff := atSync(t, src, paged, pageReq)
	if pageDiff.Nodes != diff.Nodes || pageDiff.Bytes != diff.Bytes {
		t.Fatalf("paged diff %+v != %+v", pageDiff, diff)
	}
	if diff.Nodes != wantNodes || diff.Bytes != wantBytes || len(diff.Have) == 0 || diff.Bytes >= full.Bytes/4 {
		t.Fatalf("diff %+v want %d/%d; full %+v", diff, wantNodes, wantBytes, full)
	}
	t.Logf("full %d frames/%d bytes; diff %d frames/%d bytes, %d have markers; %d empty intermediates", full.Nodes, full.Bytes, diff.Nodes, diff.Bytes, len(diff.Have), empty)
	// Repo path is one key, not directory components; range listing and absence proofs.
	for _, q := range []TreeRequest{
		{Root: head, Profile: ProfileAtrepo, Scope: ScopeEntry, Path: []string{"app.bsky.feed.post/064"}},
		{Root: head, Profile: ProfileAtrepo, Scope: ScopeSubtree, NameRange: &NameRange{Start: "app.bsky.feed.post/064", End: "app.bsky.feed.post/065"}},
	} {
		one := mst.MemStore{}
		atSync(t, src, one, q)
		if _, ok := one[c]; !ok {
			t.Fatal("selected record absent")
		}
		records := 0
		for _, b := range one {
			if v, err := drisl.Decode(b); err == nil {
				if m, ok := v.(map[string]any); ok && m["$type"] != nil {
					records++
				}
			}
		}
		if records != 1 {
			t.Fatalf("selected %d records", records)
		}
	}
	req.Have = nil
	req.Path = []string{"app.bsky.feed.post/absent"}
	req.Limit = 1
	absent := mst.MemStore{}
	atSync(t, src, absent, req)
	for _, b := range absent {
		v, _ := drisl.Decode(b)
		if m, ok := v.(map[string]any); ok && m["$type"] != nil {
			t.Fatal("record in absence proof")
		}
	}
}

func FuzzAtrepoTreeStream(f *testing.F) {
	st, _, root := atFixture(f)
	req := TreeRequest{Root: root, Profile: ProfileAtrepo, Scope: ScopeSubtree}
	f.Add(atBody(f, st, req))
	page := req
	page.Limit = 3
	f.Add(atBody(f, st, page))
	f.Add([]byte{1, 2, 2, 0, 0xa0})
	f.Fuzz(func(t *testing.T, b []byte) {
		dst := mst.MemStore{}
		_, _ = readTree(bufio.NewReader(bytes.NewReader(b)), dst, &req)
		for c, b := range dst {
			if c.Verify(b) != nil {
				t.Fatal("unverified block stored")
			}
		}
	})
}
