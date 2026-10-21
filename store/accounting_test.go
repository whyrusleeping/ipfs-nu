package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
)

// recount recomputes the physical classes from the rows and files, bypassing the usage
// counters, so a test can check the triggers kept them exact.
func recount(t *testing.T, s *Store) Stats {
	t.Helper()
	var st Stats
	rows, err := s.db.Query(`SELECT size, outboard, src_path IS NULL FROM blobs`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var size int64
		var ob, copied bool
		if err := rows.Scan(&size, &ob, &copied); err != nil {
			t.Fatal(err)
		}
		if copied {
			st.Blobs += size
		} else {
			st.NoCopy += size
		}
		if ob {
			st.Outboards += int64(blob.OutboardSize(uint64(size)))
		}
	}
	rows.Close()
	rows, err = s.db.Query(`SELECT size, groups FROM partials`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var size int64
		var groups []byte
		if err := rows.Scan(&size, &groups); err != nil {
			t.Fatal(err)
		}
		st.Partials += heldBytes(groups, uint64(size))
		st.Outboards += int64(blob.OutboardSize(uint64(size)))
	}
	rows.Close()
	if err := s.db.QueryRow(`SELECT coalesce(sum(length(data)), 0) FROM blocks`).Scan(&st.Blocks); err != nil {
		t.Fatal(err)
	}
	return st
}

// pat yields n deterministic bytes, different for each offset.
func pat(off, n int64) *patternReader { return &patternReader{off: off, n: off + n} }

func sameClasses(a, b Stats) bool {
	return a.Blobs == b.Blobs && a.Outboards == b.Outboards && a.Partials == b.Partials && a.Blocks == b.Blocks && a.NoCopy == b.NoCopy
}

// partialOf prepares a BLAKE3 blob for range fetching into a partial.
func partialOf(t *testing.T, size uint64) (src []byte, ob obuf, mh cid.Multihash, fetch func(p *Partial, r blob.Range)) {
	t.Helper()
	src = make([]byte, size)
	for i := range src {
		src[i] = byte(i * 7 >> 3)
	}
	ob = make(obuf, blob.OutboardSize(size))
	mh, err := blob.Outboard(ob, bytes.NewReader(src), size)
	if err != nil {
		t.Fatal(err)
	}
	fetch = func(p *Partial, r blob.Range) {
		var sl bytes.Buffer
		if err := blob.ExtractSlice(&sl, bytes.NewReader(src), ob, size, r); err != nil {
			t.Fatal(err)
		}
		if err := blob.DecodeSlice(&sl, mh, size, r, p.Sink()); err != nil {
			t.Fatal(err)
		}
	}
	return src, ob, mh, fetch
}

// The usage counters track every class exactly through adds, --nocopy references,
// partial flushes, commits, source invalidation and GC (A1, A3); Info.Physical charges
// each object once.
func TestAccountingCounters(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	check := func(step string) Stats {
		t.Helper()
		st, err := s.Stats()
		if err != nil {
			t.Fatal(err)
		}
		if rc := recount(t, s); !sameClasses(st, rc) {
			t.Fatalf("%s: counters %+v, recount %+v", step, st, rc)
		}
		if st.Bytes != st.Blobs+st.Outboards+st.Partials+st.DB+st.Staging {
			t.Fatalf("%s: Bytes %d is not the sum of the classes", step, st.Bytes)
		}
		return st
	}
	check("empty")

	// A copied blob with an outboard and an inline block.
	big := int64(3*blob.GroupSize + 5)
	c, err := s.AddBlob(&patternReader{n: big}, big)
	if err != nil {
		t.Fatal(err)
	}
	small, _ := s.AddBlob(bytes.NewReader([]byte("inline")), 6)
	st := check("add")
	if st.Blobs != big || st.Outboards != int64(blob.OutboardSize(uint64(big))) || st.Blocks != 6 {
		t.Fatalf("after add: %+v", st)
	}
	info, _, _ := s.Has(c)
	if info.Physical != uint64(big)+blob.OutboardSize(uint64(big)) {
		t.Fatalf("physical of a copied blob: %d", info.Physical)
	}

	// A --nocopy source: outboard charged, bytes reported as nocopy, then invalidated.
	srcPath := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(srcPath, bytes.Repeat([]byte{9}, 2*blob.GroupSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	nc, err := s.AddNoCopy(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	st = check("nocopy")
	if st.NoCopy != 2*blob.GroupSize+1 || st.Blobs != big {
		t.Fatalf("after nocopy: %+v", st)
	}
	if info, _, _ := s.Has(nc); info.Physical != blob.OutboardSize(2*blob.GroupSize+1) {
		t.Fatalf("physical of a nocopy blob: %d", info.Physical)
	}
	if err := os.WriteFile(srcPath, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Has(nc); ok {
		t.Fatal("changed source still held")
	}
	if st = check("invalidated"); st.NoCopy != 0 {
		t.Fatalf("invalidated source still counted: %+v", st)
	}

	// A partial blob is charged by its verified groups, then as a blob once committed.
	psize := uint64(8*blob.GroupSize + 100)
	_, _, mh, fetch := partialOf(t, psize)
	p, err := s.Partial(mh, psize)
	if err != nil {
		t.Fatal(err)
	}
	fetch(p, blob.Range{Start: 0, End: 2})
	fetch(p, blob.Range{Start: 8, End: 9}) // the short last group
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	st = check("partial")
	if st.Partials != 2*blob.GroupSize+100 || st.Reserved != int64(psize)-st.Partials {
		t.Fatalf("after partial flush: %+v", st)
	}
	if info, _, _ := s.Has(cid.New(cid.Raw, mh)); info.Physical != 2*blob.GroupSize+100+blob.OutboardSize(psize) {
		t.Fatalf("physical of a partial: %d", info.Physical)
	}
	fetch(p, blob.Range{Start: 2, End: 8})
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	st = check("committed")
	if st.Partials != 0 || st.Reserved != 0 || st.Blobs != big+int64(psize) {
		t.Fatalf("after commit: %+v", st)
	}

	// GC removes the unpinned objects and their bytes; the pinned blob stays.
	if err := s.Pin(Pin{CID: c, Mode: Direct, Source: SourceExplicit}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(ctx, GCOptions{}); err != nil {
		t.Fatal(err)
	}
	st = check("gc")
	if st.Blobs != big || st.Blocks != 0 || st.Objects != 1 {
		t.Fatalf("after gc: %+v", st)
	}
	_ = small

	// Counters survive reopening (they live in the database), and migration 3 rebuilds
	// them from a schema-2 database, including the verified bytes of a partial.
	_, _, mh2, fetch2 := partialOf(t, psize)
	p2, err := s.Partial(mh2, psize)
	if err != nil {
		t.Fatal(err)
	}
	fetch2(p2, blob.Range{Start: 1, End: 4})
	if err := p2.Close(); err != nil {
		t.Fatal(err)
	}
	want := check("second partial")
	s.Close()
	s = open(t, dir)
	if got := check("reopened"); !sameClasses(got, want) {
		t.Fatalf("reopened %+v, want %+v", got, want)
	}
	for _, q := range []string{`DROP TRIGGER usage_blocks_i`, `DROP TRIGGER usage_blocks_d`, `DROP TRIGGER usage_blobs_i`,
		`DROP TRIGGER usage_blobs_d`, `DROP TRIGGER usage_partials_i`, `DROP TRIGGER usage_partials_d`, `DROP TRIGGER usage_partials_u`,
		`DROP TABLE usage`, `ALTER TABLE partials DROP COLUMN held`, `PRAGMA user_version = 2`} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s = open(t, dir)
	if got := check("migrated"); !sameClasses(got, want) || got.Partials != 3*blob.GroupSize {
		t.Fatalf("migrated %+v, want %+v", got, want)
	}
}

// Admission (A6/A7): a write that does not fit is refused with the shortfall, after one
// reclaim of unpinned content, leaving nothing visible; pinned content is never released
// to make room; a false declared size is refused before any byte is written.
func TestBudgetAdmission(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	blobSize := int64(64 * blob.GroupSize) // large next to the WAL growth counted in DB
	phys := blobSize + int64(blob.OutboardSize(uint64(blobSize)))
	st, _ := s.Stats()
	// Room for the DB plus one such blob and a bit, not two.
	limit := st.DB + phys + phys/2
	s.SetLimits(Limits{Physical: limit})

	c1, err := s.AddBlob(&patternReader{n: blobSize}, blobSize)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Pin(Pin{CID: c1, Mode: Direct, Source: SourceExplicit}); err != nil {
		t.Fatal(err)
	}
	// The second does not fit; the pinned first is not sacrificed.
	_, err = s.AddBlob(pat(1, blobSize), blobSize)
	var be *BudgetError
	if !errors.As(err, &be) || be.Reason != "store_limit" || be.Need != phys || be.Headroom >= phys || be.Limit != limit {
		t.Fatalf("second add: %v", err)
	}
	if _, ok, _ := s.Has(c1); !ok {
		t.Fatal("pinned blob released under pressure")
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "staging")); len(ents) != 0 {
		t.Fatal("refused add left staging")
	}
	st, _ = s.Stats()
	if st.Pressure || st.Headroom <= 0 || st.Headroom >= phys {
		t.Fatalf("status after refusal: %+v", st)
	}

	// Unpinning the first and admitting again reclaims it (pressure GC, once the grace
	// and the pressure-GC interval have passed) and succeeds.
	if _, err := s.Unpin(Pin{CID: c1, Mode: Direct, Source: SourceExplicit}); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(2 * s.Limits().Grace) }
	c2, err := s.AddBlob(pat(1, blobSize), blobSize)
	if err != nil {
		t.Fatalf("add after unpin: %v", err)
	}
	if _, ok, _ := s.Has(c1); ok {
		t.Fatal("unpinned blob survived pressure GC")
	}
	if info, ok, _ := s.Has(c2); !ok || info.Physical != uint64(phys) {
		t.Fatal("second blob not held")
	}

	// A partial whose declared size cannot fit is refused before anything exists.
	_, _, mh, _ := partialOf(t, uint64(2*blobSize))
	_, err = s.Partial(mh, uint64(2*blobSize))
	if !errors.As(err, &be) || be.Need != 2*blobSize+int64(blob.OutboardSize(uint64(2*blobSize))) {
		t.Fatalf("oversized partial: %v", err)
	}
	if _, ok, _ := s.Has(cid.New(cid.Raw, mh)); ok {
		t.Fatal("refused partial left a row")
	}
	if _, err := os.Stat(s.partialPath(mh)); err == nil {
		t.Fatal("refused partial left a file")
	}
	if _, err := s.Partial(mh, MaxBlobSize+1); err == nil || errors.As(err, &be) {
		t.Fatalf("size above MaxBlobSize: %v", err)
	}

	// Blocks are admitted too; a store at its limit reports pressure. Unlimited stores
	// admit everything.
	st, _ = s.Stats()
	s.SetLimits(Limits{Physical: st.Bytes})
	block := bytes.Repeat([]byte{1}, blob.GroupSize)
	if err := s.Put(cid.New(cid.Raw, cid.Sum(cid.BLAKE3, block)), block); !errors.As(err, &be) {
		t.Fatalf("block beyond the limit: %v", err)
	}
	if st, _ = s.Stats(); !st.Pressure || st.Headroom != 0 {
		t.Fatalf("no pressure at the limit: %+v", st)
	}
	s.SetLimits(Limits{})
	if _, err := s.AddBlob(pat(2, blobSize), blobSize); err != nil {
		t.Fatalf("unlimited: %v", err)
	}
	// GCIfNeeded runs only above the watermark.
	if ran, _, err := s.GCIfNeeded(ctx); ran || err != nil {
		t.Fatalf("GCIfNeeded without limits ran=%v err=%v", ran, err)
	}
	s.SetLimits(Limits{Physical: 1})
	if ran, _, err := s.GCIfNeeded(ctx); !ran || err != nil {
		t.Fatalf("GCIfNeeded above the watermark ran=%v err=%v", ran, err)
	}
}

// Attribution (A4): bytes reachable from one scope are unique to it, from two shared,
// from none unpinned.
func TestAttribution(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	n := int64(2 * blob.GroupSize)
	add := func(off int64) (cid.CID, int64) {
		c, err := s.AddBlob(pat(off, n), n)
		if err != nil {
			t.Fatal(err)
		}
		return c, n + int64(blob.OutboardSize(uint64(n)))
	}
	shared, sp := add(0)
	only1, up := add(1)
	only2, _ := add(2)
	_, unpinned := add(3)
	for _, p := range []Pin{
		{CID: shared, Mode: Direct, Source: SourceFollow, Name: "a"},
		{CID: only1, Mode: Direct, Source: SourceFollow, Name: "a"},
		{CID: shared, Mode: Direct, Source: SourceFollow, Name: "b"},
		{CID: only2, Mode: Direct, Source: SourceFollow, Name: "b"},
		{CID: only2, Mode: Direct, Source: SourceExplicit}, // a second pin of the same scope-less kind
	} {
		if err := s.Pin(p); err != nil {
			t.Fatal(err)
		}
	}
	a, err := s.Attribution(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Scopes) != 3 || a.Shared != sp+up || a.Unpinned != unpinned {
		t.Fatalf("attribution: %+v", a)
	}
	byName := map[string]ScopeUsage{}
	for _, sc := range a.Scopes {
		byName[sc.Source+"/"+sc.Name] = sc
	}
	fa, fb, ex := byName["follow/a"], byName["follow/b"], byName["explicit/"]
	if fa.Unique != up || fa.Physical != sp+up || fa.Logical != 2*n || fa.Pins != 2 {
		t.Fatalf("follow/a: %+v", fa)
	}
	if fb.Unique != 0 || fb.Physical != sp+up || ex.Unique != 0 || ex.Physical != up {
		t.Fatalf("follow/b %+v explicit %+v", fb, ex)
	}
}

// A Meter refuses the charge that would exceed its window and resets after it (T1/T2).
func TestMeter(t *testing.T) {
	now := time.Unix(1000, 0)
	m := NewMeter(100, time.Hour)
	m.now = func() time.Time { return now }
	if err := m.Charge(60); err != nil {
		t.Fatal(err)
	}
	err := m.Charge(50)
	var te *TransferError
	if !errors.As(err, &te) || te.Used != 60 || te.Need != 50 || !te.ResetsAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("over budget: %v", err)
	}
	if st := m.State(); st.Used != 60 {
		t.Fatalf("refused charge counted: %+v", st)
	}
	now = now.Add(time.Hour)
	if err := m.Charge(50); err != nil || m.State().Used != 50 {
		t.Fatalf("after reset: %v %+v", err, m.State())
	}
	if err := NewMeter(0, time.Hour).Charge(1 << 40); err != nil {
		t.Fatalf("unlimited meter: %v", err)
	}
}
