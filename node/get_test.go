package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/node/link"
	"github.com/whyrusleeping/nu/nutest/timeouts"
)

// Listen config becomes link hints: unspecified addresses expand over the machine's
// addresses of the same family, loopback and link-local are dropped, concrete and DNS
// addresses are kept.
func TestExpandListen(t *testing.T) {
	ips := func() []net.IP { return []net.IP{net.ParseIP("192.168.1.7"), net.ParseIP("2001:db8::7")} }
	got := expandListen([]string{
		"/ip4/0.0.0.0/udp/4171/quic-v1", "/ip6/::/udp/4171/quic-v1", "/ip4/127.0.0.1/udp/1/quic-v1",
		"/ip6/fe80::1/udp/1/quic-v1", "/ip4/203.0.113.9/udp/4171/quic-v1", "/dns4/nu.example/udp/4171/quic-v1", "garbage",
	}, ips)
	want := []string{"/ip4/192.168.1.7/udp/4171/quic-v1", "/ip6/2001:db8::7/udp/4171/quic-v1", "/ip4/203.0.113.9/udp/4171/quic-v1", "/dns4/nu.example/udp/4171/quic-v1"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("addr %d: %s, want %s", i, got[i], want[i])
		}
	}
}

// fakeFetcher records the request and "fetches" by copying from another node's store.
type fakeFetcher struct {
	src, dst *Local
	root     cid.CID
	hints    []link.Provider
}

func (f *fakeFetcher) Fetch(ctx context.Context, root cid.CID, hints []link.Provider, progress func(done, total int64)) error {
	f.root, f.hints = root, hints
	_, err := f.src.Store.Walk(ctx, pinFor(root, ""), func(c cid.CID, held bool) error {
		if c.Codec() == cid.Raw {
			b, err := f.src.Store.OpenBlob(c.Multihash())
			if err != nil {
				return err
			}
			defer b.Close()
			_, err = f.dst.Store.AddBlobHash(io.NewSectionReader(b, 0, int64(b.Size)), int64(b.Size), c.Hash())
			return err
		}
		data, err := f.src.Store.Get(c)
		if err != nil {
			return err
		}
		return f.dst.Store.Put(c, data)
	})
	progress(1, 1)
	return err
}

// get of content the node does not hold asks the Fetcher with the link's hints, then
// extracts; the result says it was fetched.
func TestGetAsksFetcher(t *testing.T) {
	holder, getter := newLocal(t), newLocal(t)
	ff := &fakeFetcher{src: holder, dst: getter}
	getter.Fetcher = ff
	ctx := context.Background()

	f := filepath.Join(t.TempDir(), "hello.txt")
	os.WriteFile(f, []byte("hello"), 0o644)
	res, err := add(t, holder, apiv0.AddRequest{Path: f, Pin: true})
	if err != nil {
		t.Fatal(err)
	}
	l, err := link.Parse(res.Link)
	if err != nil || l.Providers[0].Peer != holder.PeerID() {
		t.Fatalf("add link %q: %v", res.Link, err)
	}
	dir := t.TempDir()
	o, err := getter.Get(ctx, apiv0.GetRequest{Target: res.Link, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	final, err := getter.WaitOperation(ctx, o.ID, 0)
	if err != nil || final.State != apiv0.StateSucceeded {
		t.Fatalf("get: %v %+v", err, final)
	}
	var gr apiv0.GetResult
	json.Unmarshal(final.Result, &gr)
	if !gr.Fetched || gr.Output != filepath.Join(dir, "hello.txt") || gr.Kind != "file" || gr.Bytes != 5 {
		t.Fatalf("result %+v", gr)
	}
	if ff.root.String() != res.CID || len(ff.hints) != 1 || ff.hints[0].Peer != holder.PeerID() {
		t.Fatalf("fetcher asked for %s with %v", ff.root, ff.hints)
	}
	if b, _ := os.ReadFile(gr.Output); string(b) != "hello" {
		t.Fatalf("extracted %q", b)
	}
	// Share needs complete content; the getter now holds it and names itself.
	sl, err := getter.Share(ctx, apiv0.ShareRequest{Target: res.CID})
	if err != nil || sl.PeerID != getter.PeerID().Base36() || sl.Serving {
		t.Fatalf("share: %+v %v", sl, err)
	}
	// Relative output paths are refused up front.
	if _, err := getter.Get(ctx, apiv0.GetRequest{Target: res.CID, Output: "rel"}); !isCode(err, apiv0.CodeInvalidArgument) {
		t.Fatalf("relative output: %v", err)
	}
	// The key persists: a second node on the same repo has the same identity.
	again, err := NewLocal(getter.Repo)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.PeerID() != getter.PeerID() {
		t.Fatal("identity changed between nodes of one repo")
	}
}

// stallingFetcher copies everything but one blob, reports progress (the structure is
// held), waits for release and fails.
type stallingFetcher struct {
	fakeFetcher
	skip    cid.CID
	release chan struct{}
}

func (f *stallingFetcher) Fetch(ctx context.Context, root cid.CID, hints []link.Provider, progress func(done, total int64)) error {
	_, err := f.src.Store.Walk(ctx, pinFor(root, ""), func(c cid.CID, held bool) error {
		if c == f.skip {
			return nil
		}
		if c.Codec() == cid.Raw {
			b, err := f.src.Store.OpenBlob(c.Multihash())
			if err != nil {
				return err
			}
			defer b.Close()
			_, err = f.dst.Store.AddBlobHash(io.NewSectionReader(b, 0, int64(b.Size)), int64(b.Size), c.Hash())
			return err
		}
		data, err := f.src.Store.Get(c)
		if err != nil {
			return err
		}
		return f.dst.Store.Put(c, data)
	})
	if err != nil {
		return err
	}
	progress(1, 2)
	<-f.release
	return apiv0.Errorf(apiv0.CodeUnavailable, "provider went away")
}

// A get writes a new output under its partial name while it fetches: files whose bytes
// arrived are written before the fetch ends, a failed fetch leaves the partial (and no
// output), and the next get completes it and renames it into place.
func TestGetStreamsToPartial(t *testing.T) {
	holder, getter := newLocal(t), newLocal(t)
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "d")
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("aaa"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "b.txt"), []byte("bbb"), 0o644)
	res, err := add(t, holder, apiv0.AddRequest{Path: srcDir, Recursive: true, Pin: true})
	if err != nil {
		t.Fatal(err)
	}
	root, _ := cid.Parse(res.CID)
	b, err := holder.content.Resolve(root, "b.txt")
	if err != nil {
		t.Fatal(err)
	}
	stall := &stallingFetcher{fakeFetcher: fakeFetcher{src: holder, dst: getter}, skip: b.Blob, release: make(chan struct{})}
	getter.Fetcher = stall
	out := filepath.Join(t.TempDir(), "out")
	o, err := getter.Get(ctx, apiv0.GetRequest{Target: res.CID, Output: out})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(timeouts.Ceiling()); ; time.Sleep(10 * time.Millisecond) {
		if data, _ := os.ReadFile(filepath.Join(out+PartialSuffix, "a.txt")); string(data) == "aaa" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a.txt not written while the fetch ran")
		}
	}
	close(stall.release)
	if final, err := getter.WaitOperation(ctx, o.ID, 0); err != nil || final.State != apiv0.StateFailed {
		t.Fatalf("stalled get: %v %+v", err, final)
	}
	if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output exists after a failed get: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(out+PartialSuffix, "a.txt")); err != nil {
		t.Fatalf("partial not kept: %v", err)
	}

	getter.Fetcher = &fakeFetcher{src: holder, dst: getter}
	if o, err = getter.Get(ctx, apiv0.GetRequest{Target: res.CID, Output: out}); err != nil {
		t.Fatal(err)
	}
	if final, err := getter.WaitOperation(ctx, o.ID, 0); err != nil || final.State != apiv0.StateSucceeded {
		t.Fatalf("second get: %v %+v", err, final)
	}
	for name, want := range map[string]string{"a.txt": "aaa", "b.txt": "bbb"} {
		if data, _ := os.ReadFile(filepath.Join(out, name)); string(data) != want {
			t.Fatalf("%s: %q", name, data)
		}
	}
	if _, err := os.Lstat(out + PartialSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial left after success: %v", err)
	}
}
