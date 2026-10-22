package fetch_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/nutest/timeouts"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/proto"
	"github.com/whyrusleeping/nu/store"
)

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newHost starts a host; with st it serves /nu/bytes/1 and /nu/have/1 from it.
func newHost(t *testing.T, st *store.Store) *host.Host {
	t.Helper()
	k, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	h, err := host.New(host.Config{Key: k})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	if st != nil {
		if err := proto.NewServer(st, proto.ServerConfig{}).Register(h); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func connect(t *testing.T, a, b *host.Host) fetch.Peer {
	t.Helper()
	addr, err := wire.ParseMultiaddr(b.Addrs()[0].String()+"/p2p/"+b.ID().String(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.Ceiling())
	defer cancel()
	if _, err := a.Dial(ctx, addr); err != nil {
		t.Fatal(err)
	}
	return fetch.HostPeer(a, b.ID())
}

func randBytes(seed byte, n int) []byte {
	b := make([]byte, n)
	rand.NewChaCha8([32]byte{seed}).Read(b)
	return b
}

func readBlob(t *testing.T, st *store.Store, c cid.CID) []byte {
	t.Helper()
	b, err := st.OpenBlob(c.Multihash())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	data, err := io.ReadAll(b.Reader())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// corruptPeer flips a byte 4 KiB into every /nu/bytes/1 response it receives: inside the
// data of the first chunk group of a Bao slice, or of a whole object larger than that.
type corruptPeer struct{ fetch.Peer }

func (p corruptPeer) Open(ctx context.Context, protocol string) (proto.Stream, error) {
	s, err := p.Peer.Open(ctx, protocol)
	if err != nil || protocol != proto.BytesID {
		return s, err
	}
	return &flipStream{Stream: s}, nil
}

type flipStream struct {
	proto.Stream
	off int
}

func (s *flipStream) Read(b []byte) (int, error) {
	n, err := s.Stream.Read(b)
	if at := 4096 - s.off; at >= 0 && at < n {
		b[at] ^= 1
	}
	s.off += n
	return n, err
}

// A ~100 MiB blob (through its MASL file node) and a small tree complete from three
// peers, one of which corrupts what it sends; the corrupt peer is dropped.
func TestMultiPeerWithCorruptPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Three provider hosts serve one shared store; the third corrupts in transit.
	src := openStore(t, t.TempDir())
	data := randBytes(1, 100<<20)
	blobID, err := src.AddBlob(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	fileNode, fnData, err := drisl.Sum(tree.FileNode{Src: blobID, Nu: tree.FileMeta{Size: uint64(len(data)), Name: "big.bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Put(fileNode, fnData); err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{
		"d/a.txt":        {Data: []byte("hello")},
		"d/sub/b.bin":    {Data: randBytes(2, 300<<10)},
		"d/sub/c.bin":    {Data: randBytes(3, 10<<10)},
		"d/sub/deep/e":   {Data: nil},
		"d/sub/deep/run": {Data: []byte("#!/bin/sh\n"), Mode: 0o755},
	}
	built, err := tree.BuildFS(fsys, "d", tree.BuildOptions{Blobs: src, Nodes: src})
	if err != nil {
		t.Fatal(err)
	}

	b := newHost(t, nil)
	good1, good2 := connect(t, b, newHost(t, src)), connect(t, b, newHost(t, src))
	evil := connect(t, b, newHost(t, src))
	dst := openStore(t, t.TempDir())
	f := fetch.New(fetch.Config{Store: dst, Providers: fetch.Static{good1, corruptPeer{evil}, good2}})

	var last fetch.Progress
	res, err := f.Get(ctx, fileNode, fetch.GetOptions{Options: fetch.Options{Progress: func(p fetch.Progress) { last = p }}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Dropped, []wire.PeerID{evil.ID()}) {
		t.Fatalf("dropped %v, want only the corrupt peer %v", res.Dropped, evil.ID())
	}
	if last.Stage != fetch.StageDone || last.Bytes != int64(len(data)) || last.BytesTotal != last.Bytes || last.Objects != 2 {
		t.Fatalf("final progress %+v", last)
	}
	if !bytes.Equal(readBlob(t, dst, blobID), data) {
		t.Fatal("blob differs")
	}

	if _, err := f.Get(ctx, built.Header, fetch.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	for name, file := range fsys {
		e, err := tree.Resolve(dst, built.Root, name[len("d/"):])
		if err != nil {
			t.Fatal(name, err)
		}
		if got := readBlob(t, dst, e.CID); !bytes.Equal(got, file.Data) {
			t.Fatalf("%s differs", name)
		}
	}
}

// recordPeer records the ranges of every /nu/bytes/1 request sent through it.
type recordPeer struct {
	fetch.Peer
	mu     *sync.Mutex
	ranges *[]blob.Range
}

func (p recordPeer) Open(ctx context.Context, protocol string) (proto.Stream, error) {
	s, err := p.Peer.Open(ctx, protocol)
	if err != nil || protocol != proto.BytesID {
		return s, err
	}
	return &recordStream{Stream: s, p: p}, nil
}

type recordStream struct {
	proto.Stream
	p   recordPeer
	buf []byte
}

func (s *recordStream) Write(b []byte) (int, error) {
	s.buf = append(s.buf, b...)
	if msg, err := wire.ReadMessage(bytes.NewReader(s.buf), proto.MaxRequestSize); err == nil {
		if req, err := proto.ParseBytesRequest(msg); err == nil {
			s.p.mu.Lock()
			*s.p.ranges = append(*s.p.ranges, req.Ranges...)
			s.p.mu.Unlock()
		}
	}
	return s.Stream.Write(b)
}

// A download interrupted part-way and restarted on a reopened store resumes: the second
// run never requests a group the first one verified.
func TestResumeAfterRestart(t *testing.T) {
	src := openStore(t, t.TempDir())
	data := randBytes(4, 32<<20)
	id, err := src.AddBlob(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	b := newHost(t, nil)
	peers := fetch.Static{connect(t, b, newHost(t, src)), connect(t, b, newHost(t, src))}
	dir := t.TempDir()

	// First run: size unknown (a bare blob CID), stopped after 12 MiB.
	st1, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = fetch.New(fetch.Config{Store: st1, Providers: peers}).Get(ctx, id, fetch.GetOptions{Options: fetch.Options{
		Progress: func(p fetch.Progress) {
			if p.Bytes >= 12<<20 {
				cancel()
			}
		}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted fetch: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}

	st2 := openStore(t, dir)
	_, held, complete, err := st2.HeldRanges(id.Multihash())
	if err != nil || complete || len(held) == 0 {
		t.Fatalf("after restart: held %v complete %v err %v", held, complete, err)
	}
	var mu sync.Mutex
	var asked []blob.Range
	var rec fetch.Static
	for _, p := range peers {
		rec = append(rec, recordPeer{Peer: p, mu: &mu, ranges: &asked})
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	if _, err := fetch.New(fetch.Config{Store: st2, Providers: rec}).Get(ctx2, id, fetch.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, a := range asked {
		for _, h := range held {
			if a.Start < h.End && h.Start < a.End {
				t.Fatalf("re-requested %v, already held %v", a, h)
			}
		}
	}
	if len(asked) == 0 || !bytes.Equal(readBlob(t, st2, id), data) {
		t.Fatalf("resumed blob differs (%d requests)", len(asked))
	}
}

// A partial blob created with a wrong size is discarded, whether the right size comes from
// the caller or is proven from the peers.
func TestWrongSizePartialDiscarded(t *testing.T) {
	src := openStore(t, t.TempDir())
	data := randBytes(5, 1<<20)
	id, err := src.AddBlob(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	b := newHost(t, nil)
	peers := fetch.Static{connect(t, b, newHost(t, src))}
	for _, known := range []uint64{uint64(len(data)), 0} {
		dst := openStore(t, t.TempDir())
		p, err := dst.Partial(id.Multihash(), 3<<20)
		if err != nil {
			t.Fatal(err)
		}
		p.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err = fetch.New(fetch.Config{Store: dst, Providers: peers}).Get(ctx, id, fetch.GetOptions{Options: fetch.Options{Size: known}})
		cancel()
		if err != nil {
			t.Fatalf("size %d: %v", known, err)
		}
		if !bytes.Equal(readBlob(t, dst, id), data) {
			t.Fatal("blob differs")
		}
	}
}
