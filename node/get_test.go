package node

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/node/link"
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
