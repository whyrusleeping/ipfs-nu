//go:build linux || darwin

package nutest_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"syscall"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/node/link"
	"github.com/whyrusleeping/nu/nutest"
	"github.com/whyrusleeping/nu/nutest/slow"
	"github.com/whyrusleeping/nu/store"
)

// TestHardening10GB runs serial phases to keep disk use below 31 GB. Providers
// share one local nocopy source, so this measures correctness and loopback bounds,
// not independent-disk/network near-sum throughput in a deployment.
func TestHardening10GB(t *testing.T) {
	slow.Skip(t)
	const size int64 = 10_000_000_000
	dir := t.TempDir()
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		t.Fatal(err)
	}
	if uint64(stat.Bavail)*uint64(stat.Bsize) < 35_000_000_000 {
		t.Skip("10 GB campaign needs 35 GB free disk")
	}
	n := nutest.New(t, 4)
	good, other, bad, client := n.Nodes[0], n.Nodes[1], n.Nodes[2], n.Nodes[3]
	source := filepath.Join(dir, "source")
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i % 251)
	}
	h := cid.BLAKE3.New()
	for off := int64(0); off < size; {
		b := buf[:min(int64(len(buf)), size-off)]
		b[0] = byte(off >> 20)
		if _, err := f.Write(b); err != nil {
			t.Fatal(err)
		}
		_, _ = h.Write(b)
		off += int64(len(b))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	want := cid.New(cid.Raw, cid.FromHasher(cid.BLAKE3, h))
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	wait := func(nd *nutest.Node, o *apiv0.Operation, err error) *apiv0.Operation {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		done, err := nd.Local.WaitOperation(ctx, o.ID, 20*time.Minute)
		if err != nil || done.State != apiv0.StateSucceeded {
			t.Fatalf("operation: %+v %v", done, err)
		}
		return done
	}
	measure := func(name string, fn func()) time.Duration {
		t.Helper()
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		peak := before.HeapAlloc
		stop, done := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				peak = max(peak, m.HeapAlloc)
				select {
				case <-stop:
					return
				case <-ticker.C:
				}
			}
		}()
		start := time.Now()
		func() {
			defer func() { close(stop); <-done }()
			fn()
		}()
		elapsed := time.Since(start)
		t.Logf("%s bytes=%d elapsed=%s MB/s=%.2f heap_base=%d peak_heap=%d", name, size, elapsed, float64(size)/elapsed.Seconds()/1e6, before.HeapAlloc, peak)
		if (name == "add" || name == "cat" || name == "get-local") && peak-before.HeapAlloc > 48<<20 {
			t.Fatal("local streaming heap delta exceeds 48 MiB")
		}
		return elapsed
	}
	old := debug.SetMemoryLimit(128 << 20)
	defer debug.SetMemoryLimit(old)
	measure("add", func() {
		o, err := good.Local.Add(ctx, apiv0.AddRequest{Path: source, Raw: true, Pin: true})
		done := wait(good, o, err)
		var res apiv0.AddResult
		if err := json.Unmarshal(done.Result, &res); err != nil || res.CID != want.String() || res.Size != size {
			t.Fatal("add digest/size mismatch", err)
		}
	})
	check := func(r io.Reader) {
		t.Helper()
		h := cid.BLAKE3.New()
		count, err := io.Copy(h, r)
		if err != nil || count != size || cid.FromHasher(cid.BLAKE3, h) != want.Multihash() {
			t.Fatal("digest/size mismatch", err)
		}
	}
	measure("cat", func() {
		r, err := good.Local.Cat(ctx, want.String())
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		check(r)
	})
	out := filepath.Join(dir, "out")
	measure("get-local", func() {
		o, err := good.Local.Get(ctx, apiv0.GetRequest{Target: want.String(), Output: out})
		wait(good, o, err)
	})
	r, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	check(r)
	r.Close()
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	for _, nd := range []*nutest.Node{other, bad} {
		c, err := nd.Local.Store.AddNoCopy(source)
		if err != nil || c != want {
			t.Fatal("nocopy", err)
		}
		if err := nd.Local.Store.Pin(store.Pin{CID: c, Mode: store.Direct, Source: store.SourceExplicit}); err != nil {
			t.Fatal(err)
		}
	}
	fetch := func(providers ...*nutest.Node) {
		var hints []link.Provider
		for _, nd := range providers {
			hints = append(hints, nd.Provider())
		}
		if err := client.Local.Fetcher.Fetch(ctx, want, hints, nil); err != nil {
			t.Fatal(err)
		}
		r, err := client.Local.Cat(ctx, want.String())
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		check(r)
	}
	single := measure("single-peer-fetch+verify", func() { fetch(good) })
	if _, err := client.Local.Store.GC(ctx, store.GCOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, held, err := client.Local.Store.Has(want); err != nil || held {
		t.Fatal("baseline still cached", err)
	}
	bad.Corrupt(true)
	multi := measure("three-peer-one-corrupt-fetch+verify", func() { fetch(bad, good, other) })
	if bad.CorruptServed.Load() == 0 {
		t.Fatal("corrupt provider not exercised")
	}
	t.Logf("multi/single throughput ratio=%.3f; ideal two-honest sum ratio=2; corrupt_responses=%d (shared disk/loopback, includes final digest)", float64(single)/float64(multi), bad.CorruptServed.Load())
}
