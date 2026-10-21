package proto_test

import (
	"context"
	"errors"
	"fmt"

	"testing"
	"testing/fstest"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/nutest/slow"
	"github.com/whyrusleeping/nu/proto"
	"github.com/whyrusleeping/nu/store"
)

// site builds a tree of dirs×files small files into st; edit may change file contents.
func site(t *testing.T, st *store.Store, dirs, files int, edit func(d, f int) string) tree.BuildResult {
	t.Helper()
	fsys := fstest.MapFS{}
	for d := range dirs {
		for f := range files {
			s := fmt.Sprintf("file %d/%d\n", d, f)
			if edit != nil {
				s = edit(d, f)
			}
			fsys[fmt.Sprintf("site/d%03d/f%04d.txt", d, f)] = &fstest.MapFile{Data: []byte(s), Mode: 0o644}
		}
	}
	res, err := tree.BuildFS(fsys, "site", tree.BuildOptions{Blobs: st, Nodes: st})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// reach returns every object the nu-tree walk reaches from root in st.
func reach(t *testing.T, st *store.Store, root cid.CID) map[cid.CID]bool {
	t.Helper()
	seen := map[cid.CID]bool{}
	var visit func(c cid.CID)
	visit = func(c cid.CID) {
		if seen[c] {
			return
		}
		seen[c] = true
		if c.Codec() != cid.DRISL {
			return
		}
		data, err := st.Get(c)
		if err != nil {
			t.Fatal(err)
		}
		links, err := tree.Links(c, data)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range links {
			visit(l)
		}
	}
	visit(root)
	return seen
}

// syncTree repeats a request with the returned cursor until the walk is done.
func syncTree(t *testing.T, open proto.Opener, st *store.Store, req proto.TreeRequest) (total proto.TreeResult, requests int) {
	t.Helper()
	for {
		res, err := proto.FetchTree(ctxT(t), open, st, req)
		requests++
		if err != nil {
			t.Fatalf("request %d: %v", requests, err)
		}
		total.Nodes += res.Nodes
		total.Blobs += res.Blobs
		total.Bytes += res.Bytes
		total.Have = append(total.Have, res.Have...)
		total.Missing = append(total.Missing, res.Missing...)
		if !res.More {
			return total, requests
		}
		req.Cursor = res.Cursor
	}
}

func lookup(t *testing.T, n int) {
	a, b := newNode(t), newNode(t)
	big := tree.Dir(a.st, cid.CID{})
	var target tree.Entry
	for i := range n {
		e := tree.Entry{Kind: tree.KindFile, CID: cid.New(cid.Raw, cid.Sum(cid.BLAKE3, fmt.Appendf(nil, "%d", i))), Size: 10}
		if i == n/2 {
			e.CID = addBlob(t, a.st, []byte("the target"), cid.BLAKE3)
			target = e
		}
		if err := big.Put(fmt.Sprintf("f%07d", i), e); err != nil {
			t.Fatal(err)
		}
	}
	bigRoot, err := big.Flush()
	if err != nil {
		t.Fatal(err)
	}
	top := tree.Dir(a.st, cid.CID{})
	if err := top.Put("big", tree.Entry{Kind: tree.KindDir, CID: bigRoot, Size: uint64(n) * 10}); err != nil {
		t.Fatal(err)
	}
	topRoot, _ := top.Flush()
	hc, hdata, _ := drisl.Sum(tree.Header{V: 1, Root: topRoot, Files: uint64(n), Bytes: uint64(n) * 10})
	if err := a.st.Put(hc, hdata); err != nil {
		t.Fatal(err)
	}
	proof, err := tree.DirProfile.Path(a.st, bigRoot, fmt.Sprintf("f%07d", n/2), nil)
	if err != nil {
		t.Fatal(err)
	}

	open := connect(t, b.h, a.h)
	res, err := proto.FetchTree(ctxT(t), open, b.st, proto.TreeRequest{Root: hc, Path: []string{"big", fmt.Sprintf("f%07d", n/2)},
		Scope: proto.ScopeEntry, InlineMax: proto.MaxInline})
	if err != nil || res.More {
		t.Fatal(res, err)
	}
	// header, the root directory node, the search path in big, and the blob
	if res.Nodes != 2+len(proof) || res.Blobs != 1 {
		t.Fatalf("lookup in %d entries: %d nodes, %d blobs; want %d nodes", n, res.Nodes, res.Blobs, 2+len(proof))
	}
	if got := readBlob(t, b.st, target.CID); string(got) != "the target" {
		t.Fatalf("blob %q", got)
	}
	// The client verifies the entry from its own copy of the chain.
	e, err := tree.Resolve(b.st, topRoot, "big/"+fmt.Sprintf("f%07d", n/2))
	if err != nil || e != target {
		t.Fatal(e, err)
	}
}

// A scope-entry lookup in a big directory is one request for the O(log n) search path.
func TestTreeLookup(t *testing.T) { lookup(t, 20000) }

func TestSlowTreeLookup1M(t *testing.T) {
	slow.Skip(t)
	lookup(t, 1_000_000)
}

// A small edit to a 10k-file site transfers only the changed nodes and the new blob when
// the client names its old tree in have.
func TestTreeHaveDiff(t *testing.T) {
	a, b := newNode(t), newNode(t)
	old := site(t, a.st, 100, 100, nil)
	open := connect(t, b.h, a.h)
	all, requests := syncTree(t, open, b.st, proto.TreeRequest{Root: old.Header, Scope: proto.ScopeSubtree, InlineMax: proto.MaxInline})
	oldSet := reach(t, a.st, old.Header)
	if all.Nodes+all.Blobs != len(oldSet) || len(all.Missing) != 0 || requests < 3 {
		t.Fatalf("initial sync: %d nodes + %d blobs in %d requests, tree has %d objects", all.Nodes, all.Blobs, requests, len(oldSet))
	}
	for c := range oldSet {
		if _, ok, _ := b.st.Has(c); !ok {
			t.Fatalf("missing %s after the initial sync", c)
		}
	}

	edited := site(t, a.st, 100, 100, func(d, f int) string {
		if d == 42 && f == 7 {
			return "edited\n"
		}
		return fmt.Sprintf("file %d/%d\n", d, f)
	})
	var fresh []cid.CID
	for c := range reach(t, a.st, edited.Header) {
		if !oldSet[c] {
			fresh = append(fresh, c)
		}
	}
	diff, requests := syncTree(t, open, b.st, proto.TreeRequest{Root: edited.Header, Scope: proto.ScopeSubtree,
		InlineMax: proto.MaxInline, Have: []cid.CID{old.Header}})
	if requests != 1 || diff.Nodes+diff.Blobs != len(fresh) || diff.Blobs != 1 {
		t.Fatalf("diff: %d nodes + %d blobs (%d bytes) in %d requests; %d objects changed", diff.Nodes, diff.Blobs, diff.Bytes, requests, len(fresh))
	}
	for _, c := range fresh {
		if _, ok, _ := b.st.Has(c); !ok {
			t.Fatalf("changed object %s not transferred", c)
		}
	}
	t.Logf("diff: %d frames, %d bytes, %d have markers", diff.Nodes+diff.Blobs, diff.Bytes, len(diff.Have))
}

// resetAfter resets the stream once n body bytes have been read.
func resetAfter(open proto.Opener, n int64) proto.Opener {
	return func(ctx context.Context, p string) (proto.Stream, error) {
		s, err := open(ctx, p)
		if err != nil {
			return nil, err
		}
		return &resetStream{Stream: s, left: n}, nil
	}
}

type resetStream struct {
	proto.Stream
	left int64
}

func (s *resetStream) Read(b []byte) (int, error) {
	if s.left <= 0 {
		s.Stream.Reset()
		return 0, errors.New("test: reset")
	}
	b = b[:min(int64(len(b)), s.left)]
	n, err := s.Stream.Read(b)
	s.left -= int64(n)
	return n, err
}

// A response cut mid-stream resumes from the client's cursor with nothing lost and no item
// sent twice; budget stops (limit) resume the same way.
func TestTreeResume(t *testing.T) {
	a, b, c := newNode(t), newNode(t), newNode(t)
	s := site(t, a.st, 20, 30, nil)
	want := len(reach(t, a.st, s.Header))
	open := connect(t, b.h, a.h)
	req := proto.TreeRequest{Root: s.Header, Scope: proto.ScopeSubtree, InlineMax: proto.MaxInline}

	res, err := proto.FetchTree(ctxT(t), resetAfter(open, 5000), b.st, req)
	if err == nil || res.Cursor == nil || res.Nodes+res.Blobs == 0 {
		t.Fatalf("cut response: %+v %v", res, err)
	}
	got := res.Nodes + res.Blobs
	req.Cursor = res.Cursor
	rest, _ := syncTree(t, open, b.st, req)
	if got+rest.Nodes+rest.Blobs != want {
		t.Fatalf("resume: %d + %d items, tree has %d", got, rest.Nodes+rest.Blobs, want)
	}

	// Pages of 7 frames from another client.
	open = connect(t, c.h, a.h)
	req = proto.TreeRequest{Root: s.Header, Scope: proto.ScopeSubtree, InlineMax: proto.MaxInline, Limit: 7}
	pages, requests := syncTree(t, open, c.st, req)
	if pages.Nodes+pages.Blobs != want || requests != (want+6)/7 {
		t.Fatalf("paged: %d items in %d requests, tree has %d", pages.Nodes+pages.Blobs, requests, want)
	}
}

// A name range lists part of a directory with the boundary nodes needed to verify it;
// nodes requests return nodes by CID; bad cursors are refused.
func TestTreeRangeAndNodes(t *testing.T) {
	a, b := newNode(t), newNode(t)
	s := site(t, a.st, 1, 2000, nil)
	open := connect(t, b.h, a.h)
	rng := &proto.NameRange{Start: "f0500.txt", End: "f0600.txt"}
	res, err := proto.FetchTree(ctxT(t), open, b.st, proto.TreeRequest{Root: s.Header, Path: []string{"d000"}, Scope: proto.ScopeEntry,
		NameRange: rng, InlineMax: proto.MaxInline})
	if err != nil || res.Blobs != 100 {
		t.Fatalf("%+v %v", res, err)
	}
	dir, err := tree.Resolve(b.st, s.Root, "d000")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	err = tree.Dir(b.st, dir.CID).Walk(rng.Start, func(k string, e tree.Entry) (bool, error) {
		if k >= rng.End {
			return false, nil
		}
		n++
		_, err := b.st.Get(e.CID)
		return true, err
	})
	if err != nil || n != 100 {
		t.Fatalf("local walk of the range: %d entries, %v", n, err)
	}
	if total := len(reach(t, a.st, dir.CID)) - 2000; res.Nodes >= total {
		t.Fatalf("range fetched %d of %d directory nodes", res.Nodes, total)
	}

	missing := cid.New(cid.DRISL, cid.Sum(cid.SHA256, []byte("nope")))
	res, err = proto.FetchTree(ctxT(t), open, b.st, proto.TreeRequest{Nodes: []cid.CID{s.Header, missing, s.Root}, Scope: proto.ScopeNode})
	if err != nil || res.Handled != 3 || res.Nodes != 2 || len(res.Missing) != 1 || res.Missing[0] != missing {
		t.Fatalf("nodes: %+v %v", res, err)
	}

	_, err = proto.FetchTree(ctxT(t), open, b.st, proto.TreeRequest{Root: s.Header, Scope: proto.ScopeSubtree,
		Cursor: &proto.Cursor{Path: []string{"d000"}, Key: "nope", Link: proto.LinkTarget}})
	if se := (*proto.StatusError)(nil); !errors.As(err, &se) || se.Status != proto.StatusBadRequest {
		t.Fatalf("bad cursor: %v", err)
	}
	if _, err := proto.FetchTree(ctxT(t), open, b.st, proto.TreeRequest{Root: missing, Scope: proto.ScopeNode}); !errors.Is(err, proto.ErrNotFound) {
		t.Fatalf("missing root: %v", err)
	}
}
