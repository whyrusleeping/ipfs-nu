package store

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
)

// HasMany and GetMany answer exactly as Has and Get do, per requested CID, across blocks,
// inline and file blobs, absent objects and raw bytes asked for as DRISL (0077); a GC that
// removes something changes Removals.
func TestHasMany(t *testing.T) {
	s := open(t, t.TempDir())
	fsys := fstest.MapFS{}
	for i := range 30 {
		fsys[fmt.Sprintf("d/f%02d", i)] = &fstest.MapFile{Data: bytes.Repeat([]byte{byte(i)}, []int{3, 20000, 70000}[i%3])}
	}
	res, err := tree.BuildFS(fsys, "d", tree.BuildOptions{Blobs: s, Nodes: s})
	if err != nil {
		t.Fatal(err)
	}
	var cs []cid.CID
	if _, err := s.Walk(context.Background(), Pin{CID: res.Header, Mode: Recursive, Profile: "nu-tree"}, func(c cid.CID, held bool) error {
		cs = append(cs, c)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	absent, _ := cid.Parse("bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku")
	for _, c := range cs {
		if c.Codec() == cid.Raw { // the same bytes under the DRISL codec
			cs = append(cs, cid.New(cid.DRISL, c.Multihash()))
		}
	}
	cs = append(cs, absent, cs[0]) // and a duplicate
	check := func() {
		t.Helper()
		got, err := s.HasMany(cs)
		if err != nil {
			t.Fatal(err)
		}
		blocks, err := s.GetMany(cs)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			info, ok, err := s.Has(c)
			if err != nil {
				t.Fatal(err)
			}
			if g, gok := got[c]; gok != ok || g != info {
				t.Fatalf("%s: HasMany %v %+v, Has %v %+v", c, gok, g, ok, info)
			}
			data, err := s.Get(c)
			if b, ok := blocks[c]; ok != (err == nil) || !bytes.Equal(b, data) {
				t.Fatalf("%s: GetMany %v, Get %v", c, ok, err)
			}
		}
	}
	check()
	r0 := s.Removals()
	if _, err := s.GC(context.Background(), GCOptions{}); err != nil {
		t.Fatal(err)
	}
	if s.Removals() == r0 {
		t.Fatal("a GC that removed objects left Removals unchanged")
	}
	check()
}

// GC returns the database space of the rows it deletes to the filesystem (0078): nu.db
// itself (not only its WAL) shrinks by about the inline bytes removed, and the reclaim
// estimate (Reclaimer plus DBReclaimable) is within 20% of what the GC returned.
func TestGCReclaimsDB(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	reclaim, err := s.Reclaimer()
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(1, 2))
	const n, size = 400, 6 << 10
	var est int64
	for i := range n {
		b := make([]byte, size)
		for j := range b {
			b[j] = byte(r.Uint32())
		}
		c, err := s.AddBlob(bytes.NewReader(b), size)
		if err != nil {
			t.Fatal(err)
		}
		if i%4 == 0 { // keep a quarter, interleaved with the rest
			if err := s.Pin(Pin{CID: c, Mode: Direct, Source: SourceExplicit}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		info, _, _ := s.Has(c)
		est += reclaim(c, info)
	}
	dbFile := func() int64 {
		fi, err := os.Stat(filepath.Join(dir, "nu.db"))
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	slack, err := s.DBReclaimable()
	if err != nil {
		t.Fatal(err)
	}
	before, db0 := stats(t, s), dbFile()
	if _, err := s.GC(context.Background(), GCOptions{}); err != nil {
		t.Fatal(err)
	}
	after, db1 := stats(t, s), dbFile()
	if shrunk := db0 - db1; shrunk < n*3/4*size*8/10 {
		t.Fatalf("nu.db shrank %d bytes after a GC of %d inline bytes", shrunk, n*3/4*size)
	}
	freed, want := before.Bytes-after.Bytes, est+slack
	if freed < want*8/10 || freed > want*12/10 {
		t.Fatalf("GC returned %d bytes; estimated %d", freed, want)
	}
}

func stats(t *testing.T, s *Store) Stats {
	t.Helper()
	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A database created before 0078 (auto_vacuum NONE) is converted on Open, keeping its data.
func TestIncrementalVacuumUpgrade(t *testing.T) {
	dir := t.TempDir()
	db := rawDB(t, dir)
	if err := migrateTo(db, len(migrations)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO pins VALUES (x'01', 'direct', '', 'explicit', '', 0)`); err != nil {
		t.Fatal(err)
	}
	var mode, pins int
	if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil || mode != 0 {
		t.Fatalf("fixture auto_vacuum %d %v", mode, err)
	}
	db.Close()
	s := open(t, dir)
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil || mode != autoVacuumIncremental {
		t.Fatalf("auto_vacuum after Open: %d %v", mode, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM pins`).Scan(&pins); err != nil || pins != 1 {
		t.Fatalf("pins after conversion: %d %v", pins, err)
	}
}

// A tree linking a DRISL CID whose bytes are stored only as a raw blob (a "codec swap",
// 0077) does not hold that node: the walk reports it missing and never follows the links
// its bytes would have as DRISL, even when the same tree also links the raw blob.
func TestWalkCodecSwap(t *testing.T) {
	s := open(t, t.TempDir())
	root, raw, swapped, secret := codecSwapTree(t, s)
	var visited []cid.CID
	st, err := s.Walk(context.Background(), Pin{CID: root, Mode: Recursive, Profile: "nu-tree"}, func(c cid.CID, held bool) error {
		visited = append(visited, c)
		if c == swapped && held {
			t.Errorf("%s held as DRISL", c)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Complete() || st.Missing != 1 || !slices.Contains(visited, raw) || !slices.Contains(visited, swapped) || slices.Contains(visited, secret) {
		t.Fatalf("walk %+v visited %v", st, visited)
	}
}

// codecSwapTree stores a secret blob, bytes that decode as a file node pointing at it
// (stored only as a raw SHA-256 blob), and a DRISL bundle root linking those bytes both
// as raw and under the DRISL codec.
func codecSwapTree(t *testing.T, s *Store) (root, raw, swapped, secret cid.CID) {
	t.Helper()
	secret, err := s.AddBlob(bytes.NewReader([]byte("members only")), 12)
	if err != nil {
		t.Fatal(err)
	}
	evil, err := drisl.Marshal(tree.FileNode{Src: secret, Nu: tree.FileMeta{Size: 12}})
	if err != nil {
		t.Fatal(err)
	}
	if raw, err = s.AddBlobHash(bytes.NewReader(evil), int64(len(evil)), cid.SHA256); err != nil {
		t.Fatal(err)
	}
	swapped = cid.New(cid.DRISL, raw.Multihash())
	b, err := drisl.Marshal(map[string]any{"$type": tree.TypeMASL, "resources": map[string]any{
		"a": map[string]any{"src": raw}, "b": map[string]any{"src": swapped}}})
	if err != nil {
		t.Fatal(err)
	}
	if root, err = cid.SumCID(cid.DRISL, cid.SHA256, b); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(root, b); err != nil {
		t.Fatal(err)
	}
	return root, raw, swapped, secret
}
