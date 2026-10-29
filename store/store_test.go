package store

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"testing"
	"testing/fstest"
	"time"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/nutest/slow"
)

func open(t testing.TB, dir string) *Store {
	t.Helper()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// patternTable has a length coprime to the group size, so groups of a patternReader differ.
var patternTable = func() []byte {
	b := make([]byte, 1<<20+7)
	r := rand.New(rand.NewPCG(1, 1))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}()

// patternReader yields n deterministic bytes without buffering them.
type patternReader struct{ off, n int64 }

func (p *patternReader) Read(b []byte) (int, error) {
	if p.off >= p.n {
		return 0, io.EOF
	}
	b = b[:min(int64(len(b)), p.n-p.off)]
	n := 0
	for n < len(b) {
		n += copy(b[n:], patternTable[(p.off+int64(n))%int64(len(patternTable)):])
	}
	p.off += int64(n)
	return n, nil
}

// peakHeap runs fn and returns the peak live heap observed while it ran. A low memory
// limit makes the GC collect transient garbage promptly, so only retained buffers count.
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

func addAndGet(t *testing.T, size int64) {
	s := open(t, t.TempDir())
	want, _, _ := blob.Sum(&patternReader{n: size})

	var c cid.CID
	var err error
	if p := peakHeap(func() { c, err = s.AddBlob(&patternReader{n: size}, size) }); p > 48<<20 {
		t.Fatalf("add held %d bytes live for a %d-byte blob", p, size)
	}
	if err != nil {
		t.Fatal(err)
	}
	if c.Multihash() != want {
		t.Fatal("wrong CID")
	}
	b, err := s.OpenBlob(c.Multihash())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var got cid.Multihash
	var n uint64
	if p := peakHeap(func() { got, n, err = blob.Sum(b.Reader()) }); p > 48<<20 {
		t.Fatalf("get held %d bytes live", p)
	}
	if err != nil || got != want || n != uint64(size) {
		t.Fatalf("read back: %v", err)
	}
	// A range slice from the stored outboard verifies against the CID.
	r := blob.Range{Start: 7, End: 9}
	var sl bytes.Buffer
	if err := b.ExtractSlice(&sl, r); err != nil {
		t.Fatal(err)
	}
	if err := blob.DecodeSlice(&sl, c.Multihash(), uint64(size), r, blob.Sink{}); err != nil {
		t.Fatal(err)
	}
}

func TestAddGet64MiB(t *testing.T) { addAndGet(t, 64<<20) }

func TestAddGetMultiGB(t *testing.T) {
	slow.Skip(t)
	addAndGet(t, 3<<30+12345)
}

func TestSmallBlobsAndBlocks(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	small, err := s.AddBlob(bytes.NewReader([]byte("hi")), 2)
	if err != nil {
		t.Fatal(err)
	}
	sha, err := s.AddBlobHash(bytes.NewReader(make([]byte, 100000)), 100000, cid.SHA256)
	if err != nil || sha.Hash() != cid.SHA256 {
		t.Fatal(err)
	}
	if _, err := s.AddBlobHash(&patternReader{n: cid.MaxWholeObject + 1}, -1, cid.SHA256); !errors.Is(err, cid.ErrTooLarge) {
		t.Fatalf("oversized SHA-256 blob: %v", err)
	}
	for _, c := range []cid.CID{small, sha} {
		if info, ok, err := s.Has(c); !ok || !info.Complete || err != nil {
			t.Fatalf("Has(%s) = %v %v %v", c, info, ok, err)
		}
	}
	// Codec confusion (0077): bytes stored raw are not a DRISL node until stored as one;
	// then both reads work.
	drislOfSmall := cid.New(cid.DRISL, small.Multihash())
	if _, ok, _ := s.Has(drislOfSmall); ok {
		t.Fatal("raw bytes held as DRISL")
	}
	if _, err := s.Get(drislOfSmall); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of raw bytes as DRISL: %v", err)
	}
	if err := s.Put(drislOfSmall, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []cid.CID{small, drislOfSmall} {
		if b, err := s.Get(c); err != nil || string(b) != "hi" {
			t.Fatalf("Get(%s) after both puts: %q %v", c, b, err)
		}
	}
	// PutBlob rejects wrong content for a known CID.
	if _, err := s.PutBlob(sha, bytes.NewReader(make([]byte, 99999)), 99999); err == nil {
		t.Fatal("PutBlob accepted wrong bytes")
	}
	// Files are private.
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			fi, _ := d.Info()
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("%s has mode %o", p, fi.Mode().Perm())
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// The schema is versioned; a newer database is refused.
	s.Close()
	s2 := open(t, dir)
	if _, err := s2.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if _, err := Open(dir, Options{}); err == nil {
		t.Fatal("opened a newer schema")
	}
}

func TestPartialResume(t *testing.T) {
	size := uint64(20*blob.GroupSize + 77)
	src := make([]byte, size)
	for i := range src {
		src[i] = byte(i * 7 >> 3)
	}
	ob := make(obuf, blob.OutboardSize(size))
	mh, err := blob.Outboard(ob, bytes.NewReader(src), size)
	if err != nil {
		t.Fatal(err)
	}
	fetch := func(p *Partial, r blob.Range) {
		var sl bytes.Buffer
		if err := blob.ExtractSlice(&sl, bytes.NewReader(src), ob, size, r); err != nil {
			t.Fatal(err)
		}
		if err := blob.DecodeSlice(&sl, mh, size, r, p.Sink()); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	s := open(t, dir)
	p, err := s.Partial(mh, size)
	if err != nil {
		t.Fatal(err)
	}
	fetch(p, blob.Range{Start: 0, End: 5})
	fetch(p, blob.Range{Start: 12, End: 15})
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = open(t, dir)
	if _, held, complete, _ := s.HeldRanges(mh); complete || len(held) != 2 {
		t.Fatalf("held after restart: %v", held)
	}
	p, err = s.Partial(mh, size)
	if err != nil {
		t.Fatal(err)
	}
	// Held ranges can be served onward before completion.
	var onward bytes.Buffer
	if err := p.ExtractSlice(&onward, blob.Range{Start: 12, End: 14}); err != nil {
		t.Fatal(err)
	}
	if err := blob.DecodeSlice(&onward, mh, size, blob.Range{Start: 12, End: 14}, blob.Sink{}); err != nil {
		t.Fatal(err)
	}
	missing := p.Missing()
	if len(missing) != 2 || missing[0] != (blob.Range{Start: 5, End: 12}) {
		t.Fatalf("missing %v", missing)
	}
	for _, r := range missing {
		fetch(p, r)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	b, err := s.OpenBlob(mh)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	got, _ := io.ReadAll(b.Reader())
	if !bytes.Equal(got, src) {
		t.Fatal("committed blob differs")
	}
	if !bytes.Equal(readAll(t, b.Outboard(), len(ob)), ob) {
		t.Fatal("rebuilt outboard differs")
	}
}

type obuf []byte

func (o obuf) WriteAt(p []byte, off int64) (int, error) { return copy(o[off:], p), nil }
func (o obuf) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(o)) {
		return 0, io.EOF
	}
	n := copy(p, o[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func readAll(t *testing.T, r io.ReaderAt, n int) []byte {
	b := make([]byte, n)
	if _, err := r.ReadAt(b, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return b
}

// GC never removes anything reachable from a pin, and removes what is not.
func TestGCKeepsPinned(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	r := rand.New(rand.NewPCG(3, 4))
	var fss []fstest.MapFS
	for i := range 8 {
		fsys := fstest.MapFS{}
		for j := range 1 + r.IntN(15) {
			// Shared names/contents across trees exercise dedup between pinned and unpinned.
			n := r.IntN(40)
			data := bytes.Repeat([]byte{byte(n)}, []int{3, 20000, 40000}[n%3])
			fsys[fmt.Sprintf("d/sub%d/f%d", j%3, n)] = &fstest.MapFile{Data: data}
		}
		fsys[fmt.Sprintf("d/own%d", i)] = &fstest.MapFile{Data: []byte(fmt.Sprint("tree", i))}
		fss = append(fss, fsys)
	}
	for round := range 3 {
		// (Re-)add every tree; earlier rounds collected the unpinned ones.
		var trees []tree.BuildResult
		for _, fsys := range fss {
			res, err := tree.BuildFS(fsys, "d", tree.BuildOptions{Blobs: s, Nodes: s})
			if err != nil {
				t.Fatal(err)
			}
			trees = append(trees, res)
		}
		var pinned []Pin
		for _, tr := range trees {
			switch r.IntN(3) {
			case 0:
				pinned = append(pinned, Pin{CID: tr.Header, Mode: Recursive, Profile: "nu-tree", Source: SourceExplicit})
			case 1:
				pinned = append(pinned, Pin{CID: tr.Header, Mode: Direct, Source: SourceFollow, Name: "x"})
			}
		}
		old, _ := s.Pins()
		for _, p := range old {
			_, _ = s.Unpin(p)
		}
		for _, p := range pinned {
			if err := s.Pin(p); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.GC(ctx, GCOptions{}); err != nil {
			t.Fatal(err)
		}
		keep := map[cid.Multihash]bool{}
		for _, p := range pinned {
			st, err := s.Walk(ctx, p, func(c cid.CID, held bool) error {
				keep[c.Multihash()] = true
				return nil
			})
			if err != nil || !st.Complete() {
				t.Fatalf("round %d: pin %v incomplete after GC (%+v, %v)", round, p, st, err)
			}
		}
		// Everything else is gone: unpinned headers in particular.
		for _, tr := range trees {
			if _, ok, _ := s.Has(tr.Header); ok != keep[tr.Header.Multihash()] {
				t.Fatalf("round %d: header %s present=%v, pinned=%v", round, tr.Header, ok, keep[tr.Header.Multihash()])
			}
		}
	}
}

// A process killed in the middle of a blob write leaves no visible or partial object.
func TestKillMidWrite(t *testing.T) {
	if dir := os.Getenv("NU_STORE_CRASH_DIR"); dir != "" {
		crashHelper(dir)
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestKillMidWrite$")
	cmd.Env = append(os.Environ(), "NU_STORE_CRASH_DIR="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(out)
	var done cid.CID
	for sc.Scan() {
		if c, err := cid.Parse(sc.Text()); err == nil {
			done = c
		}
		if sc.Text() == "midway" {
			break
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if !done.Defined() {
		t.Fatal("helper did not report its first blob")
	}
	s := open(t, dir)
	if info, ok, err := s.Has(done); !ok || !info.Complete || err != nil {
		t.Fatalf("completed blob lost: %v %v", ok, err)
	}
	var blobs int
	_ = filepath.WalkDir(filepath.Join(dir, "blobs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			blobs++
		}
		return err
	})
	staged, _ := os.ReadDir(filepath.Join(dir, "staging"))
	var rows int
	_ = s.db.QueryRow(`SELECT count(*) FROM blobs`).Scan(&rows)
	if blobs != 1 || rows != 1 || len(staged) != 0 { // the first blob (1 MiB: no outboard)
		t.Fatalf("after crash: %d blob files, %d rows, %d staged", blobs, rows, len(staged))
	}
	// Only the committed blob is charged (A1): the orphan and the emptied staging are not.
	if st, err := s.Stats(); err != nil || st.Blobs != 1<<20 || st.Objects != 1 || st.Staging != 0 {
		t.Fatalf("accounting after crash: %+v %v", st, err)
	}
}

// stallReader produces n bytes, announces "midway" after half, then blocks forever.
type stallReader struct {
	patternReader
	said bool
}

func (r *stallReader) Read(b []byte) (int, error) {
	if r.off >= r.n/2 {
		if !r.said {
			fmt.Println("midway")
			r.said = true
		}
		select {}
	}
	return r.patternReader.Read(b[:min(len(b), int(r.n/2-r.off))])
}

func crashHelper(dir string) {
	s, err := Open(dir, Options{})
	if err != nil {
		panic(err)
	}
	c, err := s.AddBlob(&patternReader{n: 1 << 20}, 1<<20)
	if err != nil {
		panic(err)
	}
	fmt.Println(c)
	_, _ = s.AddBlob(&stallReader{patternReader: patternReader{n: 4 << 20}}, 4<<20)
}

func TestNoCopy(t *testing.T) {
	s := open(t, t.TempDir())
	path := filepath.Join(t.TempDir(), "data.bin")
	data := make([]byte, 5*blob.GroupSize)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := s.AddNoCopy(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, ok, _ := s.Has(c); !ok || !info.NoCopy || !info.Complete {
		t.Fatal("nocopy blob not held")
	}
	// Touching without changing content keeps it (re-hash on mtime mismatch).
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(path, later, later)
	if _, ok, _ := s.Has(c); !ok {
		t.Fatal("touched source lost")
	}
	// A truncated source is no longer advertised, and reads fail.
	if err := os.Truncate(path, int64(len(data)-1)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Has(c); ok {
		t.Fatal("truncated nocopy source still advertised")
	}
	if _, err := s.OpenBlob(c.Multihash()); err == nil {
		t.Fatal("opened a changed nocopy source")
	}
	// Same size, different bytes: also invalid.
	data[100] ^= 1
	_ = os.WriteFile(path, data, 0o644)
	data[100] ^= 1
	c2, err := s.AddNoCopy(path)
	if err != nil || c2 == c {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(path, os.O_WRONLY, 0)
	_, _ = f.WriteAt([]byte{data[100] ^ 2}, 100)
	f.Close()
	_ = os.Chtimes(path, later.Add(time.Hour), later.Add(time.Hour))
	if _, ok, _ := s.Has(c2); ok {
		t.Fatal("corrupted nocopy source still advertised")
	}
}
