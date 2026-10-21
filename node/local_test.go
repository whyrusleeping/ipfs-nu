package node

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/nutest/slow"
	"github.com/whyrusleeping/nu/store"
)

func newLocal(t *testing.T) *Local {
	t.Helper()
	r, _, err := repo.Open(filepath.Join(t.TempDir(), "nu"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := NewLocal(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

// add runs an add to completion and returns its result.
func add(t *testing.T, n *Local, req apiv0.AddRequest) (apiv0.AddResult, error) {
	t.Helper()
	ctx := context.Background()
	o, err := n.Add(ctx, req)
	if err != nil {
		return apiv0.AddResult{}, err
	}
	final, err := n.WaitOperation(ctx, o.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != apiv0.StateSucceeded {
		return apiv0.AddResult{}, final.Error
	}
	var res apiv0.AddResult
	if err := json.Unmarshal(final.Result, &res); err != nil {
		t.Fatal(err)
	}
	return res, nil
}

func isCode(err error, code apiv0.Code) bool {
	var ae *apiv0.Error
	return errors.As(err, &ae) && ae.Code == code
}

// A directory becomes a pinned tree that ls/cat/dag read back; a file becomes a file
// node; unchanged files are not re-hashed on re-add; dag put/get round-trip; pins are
// listed and removed; SHA-256 blobs over 8 MiB are refused up front.
func TestAddCatLsDagPins(t *testing.T) {
	n := newLocal(t)
	ctx := context.Background()
	site := filepath.Join(t.TempDir(), "site")
	os.MkdirAll(filepath.Join(site, "sub"), 0o755)
	os.WriteFile(filepath.Join(site, "index.html"), []byte("<h1>hi</h1>"), 0o644)
	os.WriteFile(filepath.Join(site, "sub", "run.sh"), []byte("#!/bin/sh\n"), 0o755)

	res, err := add(t, n, apiv0.AddRequest{Path: site, Recursive: true, Pin: true})
	if err != nil || res.Name != "site" || res.Size != 21 || res.Files != 2 {
		t.Fatalf("add -r: %+v %v", res, err)
	}
	l, err := n.Ls(ctx, res.CID)
	if err != nil || len(l.Entries) != 2 || l.Entries[0].Name != "index.html" || l.Entries[1].Type != "dir" {
		t.Fatalf("ls: %+v %v", l, err)
	}
	rc, err := n.Cat(ctx, res.CID+"/sub/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "#!/bin/sh\n" {
		t.Fatalf("cat: %q", b)
	}
	if _, err := n.Cat(ctx, res.CID+"/missing"); !isCode(err, apiv0.CodeNotFound) {
		t.Fatalf("cat missing: %v", err)
	}
	if _, err := n.Cat(ctx, res.CID); !isCode(err, apiv0.CodeInvalidArgument) {
		t.Fatalf("cat dir: %v", err)
	}
	doc, err := n.DagGet(ctx, res.CID)
	if err != nil || !strings.Contains(string(doc), `"$type":"io.ipfs.nu.tree"`) || !strings.Contains(string(doc), `"files":2`) {
		t.Fatalf("dag get header: %s %v", doc, err)
	}
	// A path reaches through links into the root directory node; the executable's entry
	// keeps its mode.
	doc, err = n.DagGet(ctx, res.CID+"/root/e")
	if err != nil || !strings.HasPrefix(string(doc), `[{"k":"index.html"`) {
		t.Fatalf("dag get root/e: %s %v", doc, err)
	}
	if sub, err := n.DagGet(ctx, l.Entries[1].CID+"/e/0/v/mode"); err != nil || string(sub) != "493" {
		t.Fatalf("run.sh mode: %s %v", sub, err)
	}

	// Re-adding hits the import cache (same CID, no re-hash).
	src := store.Source{Path: filepath.Join(site, "index.html")}
	fi, _ := os.Stat(src.Path)
	src.Size, src.Mtime, src.Inode = fi.Size(), fi.ModTime().UnixNano(), inode(fi)
	if _, _, ok, err := n.Store.ImportCacheGet(src, cid.BLAKE3); !ok || err != nil {
		t.Fatalf("import cache miss after add: %v", err)
	}
	again, err := add(t, n, apiv0.AddRequest{Path: site, Recursive: true, Pin: true})
	if err != nil || again.CID != res.CID {
		t.Fatalf("re-add: %+v %v", again, err)
	}

	// A single file is a MASL node naming its blob; --raw is the blob itself.
	f, err := add(t, n, apiv0.AddRequest{Path: filepath.Join(site, "index.html"), Pin: true})
	if err != nil || f.Name != "index.html" {
		t.Fatalf("add file: %+v %v", f, err)
	}
	raw, err := add(t, n, apiv0.AddRequest{Path: filepath.Join(site, "index.html"), Raw: true})
	if err != nil || raw.CID != l.Entries[0].CID {
		t.Fatalf("add --raw: %+v %v (want %s)", raw, err, l.Entries[0].CID)
	}
	doc, _ = n.DagGet(ctx, f.CID+"/src")
	if string(doc) != `{"$link":"`+raw.CID+`"}` {
		t.Fatalf("file node src: %s", doc)
	}
	rc, _ = n.Cat(ctx, f.CID)
	b, _ = io.ReadAll(rc)
	rc.Close()
	l, _ = n.Ls(ctx, f.CID)
	if string(b) != "<h1>hi</h1>" || len(l.Entries) != 1 || l.Entries[0].Name != "index.html" || l.Entries[0].Type != "file" {
		t.Fatalf("file node cat/ls: %q %+v", b, l)
	}
	sha, err := add(t, n, apiv0.AddRequest{Path: filepath.Join(site, "index.html"), Raw: true, Hash: "sha256"})
	if err != nil || !strings.HasPrefix(sha.CID, "bafkrei") {
		t.Fatalf("sha256: %+v %v", sha, err)
	}
	big := filepath.Join(t.TempDir(), "big")
	os.WriteFile(big, nil, 0o644)
	os.Truncate(big, cid.MaxWholeObject+1)
	if _, err := n.Add(ctx, apiv0.AddRequest{Path: big, Hash: "sha256"}); !isCode(err, apiv0.CodeInvalidArgument) {
		t.Fatalf("sha256 over 8 MiB: %v", err)
	}

	// dag put/get round-trip through the strict DRISL encoder.
	put, err := n.DagPut(ctx, strings.NewReader(`{"b": 2, "a": {"$link": "`+raw.CID+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	doc, err = n.DagGet(ctx, put.CID+"/a")
	if err != nil || string(doc) != `{"$link":"`+raw.CID+`"}` {
		t.Fatalf("dag get path: %s %v", doc, err)
	}
	if _, err := n.DagPut(ctx, strings.NewReader(`{"x": {"$link": "nope"}}`)); !isCode(err, apiv0.CodeInvalidArgument) {
		t.Fatalf("dag put bad link: %v", err)
	}

	// Pins: the tree, the file node and the raw blob (direct); rm removes; pin add of
	// absent content is not_found.
	page, err := n.Pins(ctx, "", 0)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("pins: %+v %v", page, err)
	}
	o, _ := n.PinAdd(ctx, apiv0.PinRequest{CID: raw.CID, Name: "blob"})
	if fin, _ := n.WaitOperation(ctx, o.ID, 0); fin.State != apiv0.StateSucceeded {
		t.Fatalf("pin add: %+v", fin)
	}
	page, _ = n.Pins(ctx, "", 0)
	if len(page.Items) != 3 || page.Items[2].Mode != "direct" || page.Items[2].Name != "blob" {
		t.Fatalf("pins after add: %+v", page.Items)
	}
	if err := n.PinRm(ctx, f.CID); err != nil {
		t.Fatal(err)
	}
	if err := n.PinRm(ctx, f.CID); !isCode(err, apiv0.CodeNotFound) {
		t.Fatalf("pin rm twice: %v", err)
	}
	o, _ = n.PinAdd(ctx, apiv0.PinRequest{CID: "bafkreiccldh766hwcnuxnf2wh6jgzepf2nlu2lvcllt63eww5p6chi4ity"})
	if fin, _ := n.WaitOperation(ctx, o.ID, 0); fin.State != apiv0.StateFailed || fin.Error.Code != apiv0.CodeNotFound {
		t.Fatalf("pin absent: %+v", fin)
	}
	st, _ := n.Status(ctx)
	if st.Store == nil || st.Store.Pins != 2 || st.Store.Objects < 5 || st.Store.Bytes == 0 {
		t.Fatalf("status store: %+v", st.Store)
	}
}

// peakHeap runs fn and returns the peak live heap observed while it ran.
func peakHeap(fn func()) uint64 {
	old := debug.SetMemoryLimit(40 << 20)
	defer debug.SetMemoryLimit(old)
	runtime.GC()
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	base := sample[0].Value.Uint64()
	var peak uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(time.Millisecond)
		defer t.Stop()
		for {
			metrics.Read(sample)
			peak = max(peak, sample[0].Value.Uint64())
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
	fn()
	close(stop)
	<-done
	if peak < base {
		return 0
	}
	return peak - base
}

func addCatBounded(t *testing.T, size int64) {
	n := newLocal(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "big.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i * 7)
	}
	for written := int64(0); written < size; {
		chunk := buf[:min(int64(len(buf)), size-written)]
		chunk[0] = byte(written >> 20) // groups differ
		f.Write(chunk)
		h.Write(chunk)
		written += int64(len(chunk))
	}
	f.Close()
	want := h.Sum(nil)

	var res apiv0.AddResult
	var progress int
	if p := peakHeap(func() {
		o, err := n.Add(ctx, apiv0.AddRequest{Path: path, Pin: true})
		if err != nil {
			t.Fatal(err)
		}
		st, _ := n.OperationEvents(ctx, o.ID, 0)
		for {
			ev, err := st.Next(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			if ev.Type == apiv0.EventProgress && ev.Progress.Unit == "bytes" {
				progress++
			}
			if ev.Type == apiv0.EventDone {
				if ev.Operation.State != apiv0.StateSucceeded {
					t.Fatalf("add: %+v", ev.Operation.Error)
				}
				json.Unmarshal(ev.Operation.Result, &res)
			}
		}
		st.Close()
	}); p > 48<<20 {
		t.Fatalf("add held %d bytes live for a %d-byte file", p, size)
	}
	if res.Size != size || progress < 2 {
		t.Fatalf("add result %+v, %d progress events", res, progress)
	}
	h.Reset()
	if p := peakHeap(func() {
		rc, err := n.Cat(ctx, res.CID)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		if n, err := io.Copy(h, rc); err != nil || n != size {
			t.Fatalf("cat: %d %v", n, err)
		}
	}); p > 48<<20 {
		t.Fatalf("cat held %d bytes live", p)
	}
	if string(h.Sum(nil)) != string(want) {
		t.Fatal("cat returned different bytes")
	}
}

func TestAddCat64MiB(t *testing.T) { addCatBounded(t, 64<<20) }

func TestAddCatMultiGB(t *testing.T) {
	slow.Skip(t)
	addCatBounded(t, 3<<30+12345)
}

// A store budget from config refuses an add that does not fit with insufficient_storage,
// the shortfall in details and an action, keeps what is pinned, and shows pressure and
// headroom in status (D17 A5–A8).
func TestStoreBudget(t *testing.T) {
	ctx := context.Background()
	r, _, err := repo.Open(filepath.Join(t.TempDir(), "nu"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ConfigSet("store.limit", "1M"); err != nil {
		t.Fatal(err)
	}
	if err := r.ConfigSet("store.free_reserve", "0"); err != nil {
		t.Fatal(err)
	}
	n, err := NewLocal(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	if l := n.Store.Limits(); l.Physical != 1<<20 || l.FreeReserve != 0 || l.Grace != GCGrace {
		t.Fatalf("limits from config: %+v", l)
	}

	dir := t.TempDir()
	small, big := filepath.Join(dir, "small"), filepath.Join(dir, "big")
	os.WriteFile(small, make([]byte, 200<<10), 0o644)
	os.WriteFile(big, make([]byte, 2<<20), 0o644)
	res, err := add(t, n, apiv0.AddRequest{Path: small, Pin: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = add(t, n, apiv0.AddRequest{Path: big, Pin: true})
	var ae *apiv0.Error
	if !errors.As(err, &ae) || ae.Code != apiv0.CodeInsufficientStorage || ae.Action == "" {
		t.Fatalf("add beyond the limit: %v", err)
	}
	if ae.Details["reason"] != "store_limit" || ae.Details["need"].(int64) < 2<<20 || ae.Details["limit"].(int64) != 1<<20 {
		t.Fatalf("shortfall details: %v", ae.Details)
	}
	if rc, err := n.Cat(ctx, res.CID); err != nil {
		t.Fatalf("pinned content lost under pressure: %v", err)
	} else {
		rc.Close()
	}
	st, err := n.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Store.Limit != 1<<20 || st.Store.Headroom < 0 || st.Store.Headroom > 1<<20-200<<10 || st.Store.Blobs != 200<<10 {
		t.Fatalf("status: %+v", st.Store)
	}
	if apiv0.ExitCode(apiv0.CodeInsufficientStorage) != apiv0.ExitInsufficientStorage || apiv0.HTTPStatus(apiv0.CodeInsufficientStorage) != 507 {
		t.Fatal("insufficient_storage mapping")
	}
}
