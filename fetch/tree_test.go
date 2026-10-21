package fetch_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/proto"
	"github.com/whyrusleeping/nu/store"
)

// countPeer counts request streams per protocol; with noTree it behaves like a peer
// without /nu/tree/1.
type countPeer struct {
	fetch.Peer
	noTree bool
	mu     *sync.Mutex
	opens  map[string]int
}

func (p countPeer) Open(ctx context.Context, protocol string) (proto.Stream, error) {
	p.mu.Lock()
	p.opens[protocol]++
	p.mu.Unlock()
	if p.noTree && protocol == proto.TreeID {
		return nil, wire.ErrNotSupported
	}
	return p.Peer.Open(ctx, protocol)
}

// The fetcher takes a tree's structure and small blobs from /nu/tree/1 in one request when
// the peer supports it, and falls back to the layer-1 walk when it does not.
func TestTreeProtocolAndFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	src := openStore(t, t.TempDir())
	fsys := fstest.MapFS{"site/big.bin": {Data: randBytes(9, 300<<10)}}
	for i := range 60 {
		fsys[fmt.Sprintf("site/d%d/f%d.txt", i%6, i)] = &fstest.MapFile{Data: fmt.Appendf(nil, "file %d", i)}
	}
	built, err := tree.BuildFS(fsys, "site", tree.BuildOptions{Blobs: src, Nodes: src})
	if err != nil {
		t.Fatal(err)
	}
	srv := newHost(t, src)
	for _, noTree := range []bool{false, true} {
		b := newHost(t, nil)
		p := countPeer{Peer: connect(t, b, srv), noTree: noTree, mu: new(sync.Mutex), opens: map[string]int{}}
		dst := openStore(t, t.TempDir())
		if _, err := fetch.New(fetch.Config{Store: dst, Providers: fetch.Static{p}}).Get(ctx, built.Header, fetch.GetOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := readBlob(t, dst, mustResolve(t, dst, built, "big.bin")); len(got) != 300<<10 {
			t.Fatalf("big.bin: %d bytes", len(got))
		}
		bytesReqs := p.opens[proto.BytesID]
		if p.opens[proto.TreeID] != 1 || (!noTree && bytesReqs != 1) || (noTree && bytesReqs < 60) {
			t.Fatalf("noTree=%v: requests %v", noTree, p.opens)
		}
	}
}

func mustResolve(t *testing.T, st *store.Store, built tree.BuildResult, p string) cid.CID {
	t.Helper()
	e, err := tree.Resolve(st, built.Root, p)
	if err != nil {
		t.Fatal(err)
	}
	return e.CID
}
