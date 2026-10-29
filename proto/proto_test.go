package proto_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/nutest/slow"
	"github.com/whyrusleeping/nu/nutest/timeouts"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/proto"
	"github.com/whyrusleeping/nu/store"
)

type node struct {
	h   *host.Host
	st  *store.Store
	dir string
	srv *proto.Server
}

func newHost(t *testing.T) *host.Host {
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
	return h
}

func newNode(t *testing.T) *node {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n := &node{h: newHost(t), st: st, dir: dir, srv: proto.NewServer(st, proto.ServerConfig{})}
	if err := n.srv.Register(n.h); err != nil {
		t.Fatal(err)
	}
	return n
}

// connect dials b from a and returns an opener for requests from a to b.
func connect(t *testing.T, a *host.Host, b *host.Host) proto.Opener {
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
	return proto.HostOpener(a, b.ID())
}

func randBytes(seed uint64, n int) []byte {
	b := make([]byte, n)
	rand.NewChaCha8([32]byte{byte(seed)}).Read(b)
	return b
}

func addBlob(t *testing.T, st *store.Store, data []byte, h cid.Hash) cid.CID {
	t.Helper()
	c, err := st.AddBlobHash(bytes.NewReader(data), int64(len(data)), h)
	if err != nil {
		t.Fatal(err)
	}
	return c
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

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*timeouts.Ceiling())
	t.Cleanup(cancel)
	return ctx
}

// A 64 MiB blob: arbitrary ranges into a partial, onward serving of held ranges, have
// answers for complete/partial/missing objects, then the rest of the blob.
func TestBlobRangesAndHave(t *testing.T) {
	ctx := ctxT(t)
	a, b, c := newNode(t), newNode(t), newNode(t)
	data := randBytes(1, 64<<20)
	id := addBlob(t, a.st, data, cid.BLAKE3)
	size := uint64(len(data))
	ab, cb, ca := connect(t, b.h, a.h), connect(t, c.h, b.h), connect(t, c.h, a.h)

	p, err := b.st.Partial(id.Multihash(), size)
	if err != nil {
		t.Fatal(err)
	}
	ranges := []blob.Range{{Start: 3, End: 7}, {Start: 100, End: 101}, {Start: 4000, End: 4096}}
	if err := proto.FetchRanges(ctx, ab, p, ranges); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Held(), ranges) {
		t.Fatalf("held %v, want %v", p.Held(), ranges)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	missing := cid.Sum(cid.SHA256, []byte("nope"))
	res, err := proto.Have(ctx, cb, []proto.HaveQuery{{MH: id.Multihash()}, {MH: missing}, {MH: id.Multihash(), Window: &blob.Range{Start: 5, End: 101}}})
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Held || res[0].Complete || res[0].Size != size || !slices.Equal(res[0].Ranges, ranges) || res[1].Held ||
		!slices.Equal(res[2].Ranges, []blob.Range{{Start: 5, End: 7}, {Start: 100, End: 101}}) {
		t.Fatalf("have from partial peer: %+v", res)
	}
	if res, err := proto.Have(ctx, ca, []proto.HaveQuery{{MH: id.Multihash()}}); err != nil || !res[0].Complete || res[0].Codec != cid.Raw {
		t.Fatalf("have from complete peer: %+v %v", res, err)
	}

	// c fetches a held range from b's partial; a range b lacks is refused as not held.
	pc, err := c.st.Partial(id.Multihash(), size)
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.FetchRanges(ctx, cb, pc, []blob.Range{{Start: 4, End: 6}, {Start: 4090, End: 4096}}); err != nil {
		t.Fatal(err)
	}
	if err := proto.FetchRanges(ctx, cb, pc, []blob.Range{{Start: 0, End: 1}}); !errors.Is(err, proto.ErrNotHeld) {
		t.Fatalf("unheld range: %v", err)
	}
	if err := proto.Fetch(ctx, cb, c.st, cid.New(cid.Raw, cid.Sum(cid.BLAKE3, []byte("absent"))), -1); !errors.Is(err, proto.ErrNotFound) {
		t.Fatalf("unknown object: %v", err)
	}
	pc.Close()

	// b completes the blob from a, resuming from its partial.
	if err := proto.Fetch(ctx, ab, b.st, id, int64(size)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readBlob(t, b.st, id), data) {
		t.Fatal("fetched blob differs")
	}
}

// Whole-object transfers: SHA-256 blobs (file and inline), DRISL blocks, a small BLAKE3 blob
// and a BLAKE3 DRISL block served with an outboard computed on the fly.
func TestWholeObjects(t *testing.T) {
	ctx := ctxT(t)
	a, b := newNode(t), newNode(t)
	ba := connect(t, b.h, a.h)
	var ids []cid.CID
	ids = append(ids, addBlob(t, a.st, randBytes(2, 1<<20), cid.SHA256), addBlob(t, a.st, randBytes(3, 1000), cid.SHA256), addBlob(t, a.st, randBytes(4, 1000), cid.BLAKE3))
	for _, h := range []cid.Hash{cid.SHA256, cid.BLAKE3} {
		block, err := drisl.Marshal(map[string]any{"b": randBytes(5, 100<<10)})
		if err != nil {
			t.Fatal(err)
		}
		id, err := cid.SumCID(cid.DRISL, h, block)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.st.Put(id, block); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		if err := proto.Fetch(ctx, ba, b.st, id, -1); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		want, _ := a.st.OpenBlob(id.Multihash())
		got, err := b.st.OpenBlob(id.Multihash())
		if err != nil {
			t.Fatalf("%s not stored: %v", id, err)
		}
		wb, _ := io.ReadAll(want.Reader())
		gb, _ := io.ReadAll(got.Reader())
		want.Close()
		got.Close()
		if !bytes.Equal(wb, gb) {
			t.Fatalf("%s differs", id)
		}
	}
}

func corruptFile(t *testing.T, n *node, mh cid.Multihash, off int64) {
	t.Helper()
	h := hex.EncodeToString(mh.Bytes())
	f, err := os.OpenFile(filepath.Join(n.dir, "blobs", h[4:6], h), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b [1]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xff
	if _, err := f.WriteAt(b[:], off); err != nil {
		t.Fatal(err)
	}
}

// A peer serving corrupted data is reported as corrupt and nothing unverified is kept.
func TestCorruptTransfer(t *testing.T) {
	ctx := ctxT(t)
	a, b := newNode(t), newNode(t)
	ba := connect(t, b.h, a.h)
	big := addBlob(t, a.st, randBytes(6, 2<<20), cid.BLAKE3) // above 1 MiB: stored outboard
	sha := addBlob(t, a.st, randBytes(7, 1<<20), cid.SHA256)
	corruptFile(t, a, big.Multihash(), 300<<10)
	corruptFile(t, a, sha.Multihash(), 300<<10)

	err := proto.Fetch(ctx, ba, b.st, big, 2<<20)
	var ce *blob.CorruptError
	if !errors.Is(err, proto.ErrCorrupt) || !errors.As(err, &ce) || ce.Group != 18 {
		t.Fatalf("corrupt blob: %v", err)
	}
	if _, held, _, err := b.st.HeldRanges(big.Multihash()); err != nil || !slices.Equal(held, []blob.Range{{Start: 0, End: 18}}) {
		t.Fatalf("kept %v %v, want only the verified groups", held, err)
	}
	if err := proto.Fetch(ctx, ba, b.st, sha, -1); !errors.Is(err, proto.ErrCorrupt) {
		t.Fatalf("corrupt SHA-256 blob: %v", err)
	}
	if _, ok, _ := b.st.Has(sha); ok {
		t.Fatal("corrupt SHA-256 blob stored")
	}
}

// cancelAfter wraps an opener so the request's context is cancelled once n body bytes
// arrive.
func cancelAfter(open proto.Opener, n int64, cancel func()) proto.Opener {
	return func(ctx context.Context, p string) (proto.Stream, error) {
		s, err := open(ctx, p)
		if err != nil {
			return nil, err
		}
		return &countStream{Stream: s, left: n, cancel: cancel}, nil
	}
}

type countStream struct {
	proto.Stream
	left   int64
	cancel func()
}

func (s *countStream) Read(b []byte) (int, error) {
	n, err := s.Stream.Read(b)
	if s.left -= int64(n); s.left <= 0 {
		s.cancel()
	}
	return n, err
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(timeouts.Ceiling())
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

// Cancelling a transfer resets the stream, frees the server's request, keeps the verified
// groups, and fetching the missing ranges afterwards completes the blob.
func TestCancelAndResume(t *testing.T) {
	a, b := newNode(t), newNode(t)
	ba := connect(t, b.h, a.h)
	data := randBytes(8, 8<<20)
	id := addBlob(t, a.st, data, cid.BLAKE3)
	p, err := b.st.Partial(id.Multihash(), uint64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err = proto.FetchRanges(ctx, cancelAfter(ba, 1<<20, cancel), p, []blob.Range{{Start: 0, End: blob.Groups(uint64(len(data)))}})
	if !errors.Is(err, context.Canceled) || errors.Is(err, proto.ErrCorrupt) {
		t.Fatalf("cancelled fetch: %v", err)
	}
	waitFor(t, "server to free the request", func() bool { return a.srv.Active() == 0 })
	if len(p.Held()) == 0 || p.Complete() {
		t.Fatalf("held after cancel: %v", p.Held())
	}
	if err := proto.FetchRanges(ctxT(t), ba, p, p.Missing()); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readBlob(t, b.st, id), data) {
		t.Fatal("resumed blob differs")
	}
}

// A peer announcing a whole object over 8 MiB is refused before any body is read.
func TestOversizedWholeObject(t *testing.T) {
	evil := newHost(t)
	var sent atomic.Int64
	err := evil.Handle(proto.BytesID, 4, func(s *host.Stream) {
		if _, err := wire.ReadMessage(s, proto.MaxRequestSize); err != nil {
			return
		}
		hdr, _ := drisl.Marshal(map[string]any{"status": 0, "size": cid.MaxWholeObject + 1})
		if wire.WriteMessage(s, hdr, 1024) != nil {
			return
		}
		chunk := make([]byte, 64<<10)
		for {
			n, err := s.Write(chunk)
			sent.Add(int64(n))
			if err != nil {
				return
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	b := newNode(t)
	id := cid.New(cid.Raw, cid.Sum(cid.SHA256, []byte("whatever")))
	if err := proto.FetchWhole(ctxT(t), connect(t, b.h, evil), b.st, id); !errors.Is(err, proto.ErrCorrupt) {
		t.Fatalf("oversized whole object: %v", err)
	}
	if _, ok, _ := b.st.Has(id); ok {
		t.Fatal("stored")
	}
}

// Multi-GB transfer (NU_SLOW=1).
func TestSlowLargeBlob(t *testing.T) {
	slow.Skip(t)
	a, b := newNode(t), newNode(t)
	const size = 3 << 30
	id, err := a.st.AddBlob(io.LimitReader(rand.NewChaCha8([32]byte{9}), size), size)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := proto.Fetch(ctx, connect(t, b.h, a.h), b.st, id, size); err != nil {
		t.Fatal(err)
	}
	if info, ok, err := b.st.Has(id); err != nil || !ok || !info.Complete || info.Size != size {
		t.Fatalf("%+v %v %v", info, ok, err)
	}
}
