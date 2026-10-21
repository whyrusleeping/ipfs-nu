package tree

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/whyrusleeping/nu/core/car"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
)

const vectorFile = "../../testdata/vectors/nu-tree/vectors.json"

type vector struct {
	Name   string            `json:"name"`
	Note   string            `json:"note"`
	Root   string            `json:"root"`
	Blocks map[string]string `json:"blocks"` // cid -> hex
}

func raw(h cid.Hash, s string) cid.CID { return cid.New(cid.Raw, cid.Sum(h, []byte(s))) }

func dirVector(t *testing.T, name, note string, ents map[string]Entry) vector {
	st := mst.MemStore{}
	d := Dir(st, cid.CID{})
	for k, e := range ents {
		if err := d.Put(k, e); err != nil {
			t.Fatal(err)
		}
	}
	root, err := d.Flush()
	if err != nil {
		t.Fatal(err)
	}
	return vector{Name: name, Note: note, Root: root.String(), Blocks: hexBlocks(st)}
}

func hexBlocks(st mst.MemStore) map[string]string {
	out := map[string]string{}
	for c, b := range st {
		out[c.String()] = hex.EncodeToString(b)
	}
	return out
}

func buildVectors(t *testing.T) []vector {
	empty := dirVector(t, "empty-dir", "directory with no entries", nil)
	small := map[string]Entry{
		"a.txt":  {Kind: KindFile, CID: raw(cid.BLAKE3, "a"), Size: 1},
		"empty":  {Kind: KindDir, CID: cid.MustParse(empty.Root), Size: 0},
		"run.sh": {Kind: KindFile, CID: raw(cid.SHA256, "#!/bin/sh\n"), Size: 10, Mode: ModeExec},
		"link":   {Kind: KindSymlink, CID: raw(cid.BLAKE3, "a.txt"), Size: 5},
	}
	smallV := dirVector(t, "small-dir", "file (BLAKE3), executable file (SHA-256), empty subdirectory, symlink", small)
	many := map[string]Entry{}
	for i := range 64 {
		n := fmt.Sprintf("f%02d", i)
		many[n] = Entry{Kind: KindFile, CID: raw(cid.BLAKE3, n), Size: 3}
	}
	manyV := dirVector(t, "multi-node-dir", "64 files f00..f63, content = name; spans several MST nodes", many)

	hc, hb, err := drisl.Sum(Header{V: 1, Root: cid.MustParse(smallV.Root), Files: 2, Bytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	fc, fb, err := drisl.Sum(FileNode{Src: raw(cid.BLAKE3, "hello"), ContentType: "text/plain", Nu: FileMeta{Size: 5, Name: "hello.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	return []vector{empty, smallV, manyV,
		{Name: "header", Note: "io.ipfs.nu.tree header for small-dir", Root: hc.String(), Blocks: map[string]string{hc.String(): hex.EncodeToString(hb)}},
		{Name: "file-node", Note: "single-mode MASL file node for \"hello\"", Root: fc.String(), Blocks: map[string]string{fc.String(): hex.EncodeToString(fb)}},
	}
}

func TestVectors(t *testing.T) {
	got := buildVectors(t)
	if os.Getenv("NU_UPDATE_VECTORS") == "1" {
		b, _ := json.MarshalIndent(got, "", "  ")
		_ = os.MkdirAll(filepath.Dir(vectorFile), 0o755)
		if err := os.WriteFile(vectorFile, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(vectorFile)
	if err != nil {
		t.Fatal(err)
	}
	var want []vector
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(got) {
		t.Fatalf("%d vectors, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Root != want[i].Root || len(got[i].Blocks) != len(want[i].Blocks) {
			t.Errorf("%s: root %s (%d blocks), want %s (%d)", want[i].Name, got[i].Root, len(got[i].Blocks), want[i].Root, len(want[i].Blocks))
		}
		for c, h := range want[i].Blocks {
			if got[i].Blocks[c] != h {
				t.Errorf("%s: block %s differs", want[i].Name, c)
			}
		}
	}
	if len(want[2].Blocks) < 2 {
		t.Error("multi-node vector fits one node")
	}
}

type countStore struct {
	mst.MemStore
	gets int
}

func (c *countStore) Get(id cid.CID) ([]byte, error) {
	c.gets++
	return c.MemStore.Get(id)
}

func TestLookup100k(t *testing.T) {
	st := mst.MemStore{}
	d := Dir(st, cid.CID{})
	blob := raw(cid.BLAKE3, "x")
	const n = 100_000
	for i := range n {
		if err := d.Put(fmt.Sprintf("file-%06d.dat", i), Entry{Kind: KindFile, CID: blob, Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	root, err := d.Flush()
	if err != nil {
		t.Fatal(err)
	}
	cs := &countStore{MemStore: st}
	worst := 0
	for i := 0; i < n; i += 997 {
		cs.gets = 0
		if _, err := Resolve(cs, root, fmt.Sprintf("file-%06d.dat", i)); err != nil {
			t.Fatal(err)
		}
		worst = max(worst, cs.gets)
	}
	// log16(100k) ≈ 4.2; the expected search path is about that many nodes.
	t.Logf("%d nodes, worst lookup touched %d", len(st), worst)
	if worst > 10 {
		t.Fatalf("lookup touched %d nodes", worst)
	}
}

func TestBuildFS(t *testing.T) {
	fsys := fstest.MapFS{
		"d/a.txt":       {Data: []byte("a"), Mode: 0o644},
		"d/bin/run.sh":  {Data: []byte("#!/bin/sh\n"), Mode: 0o755},
		"d/sub/b.txt":   {Data: []byte("bb"), Mode: 0o600},
		"d/link":        {Data: []byte("a.txt"), Mode: fs.ModeSymlink},
		"d/empty":       {Mode: 0o755 | fs.ModeDir, ModTime: time.Unix(0, 0)},
		"other/ignored": {Data: []byte("zzz")},
	}
	st := mst.MemStore{}
	res, err := BuildFS(fsys, "d", BuildOptions{Blobs: HashSink{}, Nodes: st})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 || res.Bytes != 1+10+2+5 {
		t.Fatalf("result %+v", res)
	}
	e, err := Resolve(st, res.Root, "bin/run.sh")
	if err != nil || e.Mode != ModeExec || e.CID != raw(cid.BLAKE3, "#!/bin/sh\n") {
		t.Fatalf("run.sh %+v %v", e, err)
	}
	if e, _ := Resolve(st, res.Root, "link"); e.Kind != KindSymlink {
		t.Fatalf("link %+v", e)
	}
	if _, err := Resolve(st, res.Root, "sub/nope"); err == nil {
		t.Fatal("missing path resolved")
	}
	// Same content with different modtimes/permissions (other than exec) gives the same tree.
	fsys["d/sub/b.txt"] = &fstest.MapFile{Data: []byte("bb"), Mode: 0o644, ModTime: time.Now()}
	res2, _ := BuildFS(fsys, "d", BuildOptions{Blobs: HashSink{}, Nodes: st})
	if res2.Header != res.Header {
		t.Fatal("tree depends on non-exec permissions or mtimes")
	}
	// The nu-tree walk reaches every structure block and blob from the header.
	var n int
	err = car.PreOrder([]cid.CID{res.Header}, func(c cid.CID) ([]byte, error) {
		n++
		if c.Codec() == cid.Raw {
			return nil, nil
		}
		return st.Get(c)
	}, Links, func(car.Block) error { return nil })
	if err != nil || n < 9 {
		t.Fatalf("walk visited %d: %v", n, err)
	}
}

func FuzzDirNode(f *testing.F) {
	for _, v := range buildVectorsForFuzz(f) {
		for _, h := range v.Blocks {
			b, _ := hex.DecodeString(h)
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := cid.SumCID(cid.DRISL, cid.SHA256, b)
		if err != nil {
			return
		}
		_, _ = Links(c, b)
		d := Dir(mst.MemStore{c: b}, c)
		_ = d.Walk("", func(string, Entry) (bool, error) { return true, nil })
	})
}

func buildVectorsForFuzz(f *testing.F) []vector {
	raw, err := os.ReadFile(vectorFile)
	if err != nil {
		f.Fatal(err)
	}
	var v []vector
	_ = json.Unmarshal(raw, &v)
	return v
}
