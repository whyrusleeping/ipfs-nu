package proto

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"testing"
	"testing/fstest"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// treeFixture builds a small tree and returns a request for it and the response body the
// server writes (after the header).
func treeFixture(t testing.TB, limit uint64) (TreeRequest, []byte) {
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fsys := fstest.MapFS{}
	for i := range 40 {
		fsys[fmt.Sprintf("s/d%d/f%02d", i%3, i)] = &fstest.MapFile{Data: fmt.Appendf(nil, "file %d", i)}
	}
	built, err := tree.BuildFS(fsys, "s", tree.BuildOptions{Blobs: st, Nodes: st})
	if err != nil {
		t.Fatal(err)
	}
	req := TreeRequest{Root: built.Header, Scope: ScopeSubtree, InlineMax: MaxInline, Limit: limit}
	b, err := req.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	if f, err := NewServer(st, ServerConfig{}).serveTree(b, w); f != nil || err != nil {
		t.Fatal(f, err)
	}
	w.Flush()
	r := bytes.NewReader(out.Bytes())
	if _, err := wire.ReadMessage(r, MaxBytesHeaderSize); err != nil {
		t.Fatal(err)
	}
	return req, out.Bytes()[out.Len()-r.Len():]
}

// The client accepts the server's stream and refuses it with one bit flipped anywhere in
// an item.
func TestTreeStreamCorrupt(t *testing.T) {
	req, body := treeFixture(t, 0)
	res, err := readTree(bufio.NewReader(bytes.NewReader(body)), mst.MemStore{}, &req)
	if err != nil || res.More || res.Nodes == 0 || res.Blobs != 40 {
		t.Fatalf("%+v %v", res, err)
	}
	bad := bytes.Clone(body)
	bad[len(bad)-20] ^= 1 // inside the last blob
	if _, err := readTree(bufio.NewReader(bytes.NewReader(bad)), mst.MemStore{}, &req); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("flipped bit: %v", err)
	}
}

func FuzzTreeRequest(f *testing.F) {
	root := cid.New(cid.DRISL, cid.Sum(cid.SHA256, []byte("root")))
	for _, r := range []TreeRequest{
		{Root: root, Scope: ScopeSubtree},
		{Root: root, Path: []string{"a", "b"}, Scope: ScopeEntry, InlineMax: 4096, NameRange: &NameRange{Start: "m"}, Limit: 9,
			Have: []cid.CID{root}, Cursor: &Cursor{Path: []string{"a"}, Key: "k", Link: LinkRight, After: true}},
		{Nodes: []cid.CID{root}, Scope: ScopeNode},
		{Root: root, Profile: ProfileAtrepo, Path: []string{"app.bsky.feed.post/abc"}, Scope: ScopeEntry, Have: []cid.CID{root}},
		{Root: root, Profile: ProfileAtrepo, Scope: ScopeSubtree, NameRange: &NameRange{Start: "app.bsky.feed.post/", End: "app.bsky.feed.post0"}, Cursor: &Cursor{Path: []string{"t:app.bsky.feed.post/a", "l", "l"}, Link: LinkDir}},
	} {
		b, err := r.Marshal()
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseTreeRequest(b)
		if err != nil {
			return
		}
		enc, err := r.Marshal()
		if err != nil {
			t.Fatalf("valid request does not re-encode: %v", err)
		}
		r2, err := ParseTreeRequest(enc)
		if err != nil {
			t.Fatalf("round trip: %v", err)
		}
		if enc2, _ := r2.Marshal(); !bytes.Equal(enc, enc2) {
			t.Fatal("re-encoding is not stable")
		}
	})
}

// FuzzTreeStream feeds arbitrary response bodies to the client walk: it must not panic,
// and must store only items that match their CIDs.
func FuzzTreeStream(f *testing.F) {
	req, body := treeFixture(f, 0)
	_, paged := treeFixture(f, 5)
	f.Add(body)
	f.Add(paged)
	f.Add([]byte{0x01, 0x00})
	f.Fuzz(func(t *testing.T, b []byte) {
		st := mst.MemStore{}
		_, _ = readTree(bufio.NewReader(bytes.NewReader(b)), st, &req)
		for c, data := range st {
			if err := c.Verify(data); err != nil {
				t.Fatalf("stored unverified item %s", c)
			}
		}
	})
}
