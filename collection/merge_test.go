package collection

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
)

func item(v string) tree.Entry {
	return tree.Entry{Kind: tree.KindFile, CID: cid.New(cid.Raw, cid.Sum(cid.BLAKE3, []byte(v))), Size: uint64(len(v)),
		Meta: cid.New(cid.DRISL, cid.Sum(cid.SHA256, []byte("meta "+v)))}
}

// buildCatalog writes a writer tree holding leaves (prefix-free) and returns its root.
func buildCatalog(t testing.TB, st mst.Store, leaves map[string]tree.Entry) cid.CID {
	t.Helper()
	ed, err := newEditor(st, cid.CID{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(leaves))
	for k := range leaves {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if err := ed.put(split(k), leaves[k]); err != nil {
			t.Fatal(err)
		}
	}
	root, err := ed.flush()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// reference decides the union over flattened leaf maps, independently of Union.
func reference(writers []map[string]tree.Entry, drop []string) map[string]tree.Entry {
	under := func(m map[string]tree.Entry, p string) bool {
		for k := range m {
			if strings.HasPrefix(k, p+"/") || strings.HasPrefix(p, k+"/") {
				return true
			}
		}
		return false
	}
	out := map[string]tree.Entry{}
	for _, m := range writers {
	next:
		for p := range m {
			if _, done := out[p]; done {
				continue
			}
			for _, d := range drop {
				if p == d || strings.HasPrefix(p, d+"/") {
					continue next
				}
			}
			for _, w := range writers {
				if e, ok := w[p]; ok {
					out[p] = e
					break
				}
				if under(w, p) {
					break
				}
			}
		}
	}
	return out
}

// TestUnionIncremental applies random writer, rank and drop changes and checks after each
// that the incremental merged root and the reported item stream equal an independent
// reference merge.
func TestUnionIncremental(t *testing.T) {
	st := mst.MemStore{}
	rng := rand.New(rand.NewSource(1))
	paths := []string{"a", "a/1", "a/2", "a/2/x", "b/1", "b/2", "b/3", "c/d/1", "c/d/2", "c/e", "c"}
	dids := []string{"w0", "w1", "w2"}
	leaves := make([]map[string]tree.Entry, 3)
	for i := range leaves {
		leaves[i] = map[string]tree.Entry{}
	}
	view := map[string]tree.Entry{}
	u := &Union{St: st, OnItem: func(p string, old *tree.Entry, nw *Leaf) error {
		if cur, ok := view[p]; (old == nil) == ok || (old != nil && cur != *old) {
			return fmt.Errorf("stream out of sync at %s: old %v, view %v", p, old, cur)
		}
		if nw == nil {
			delete(view, p)
		} else {
			view[p] = nw.Entry
		}
		return nil
	}}
	order := []int{0, 1, 2}
	var drop []string
	for step := range 300 {
		switch r := rng.Intn(20); {
		case r == 0:
			rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		case r == 1:
			drop = nil
			if rng.Intn(2) == 0 {
				drop = []string{paths[rng.Intn(len(paths))]}
			}
		default:
			w := leaves[rng.Intn(3)]
			p := paths[rng.Intn(len(paths))]
			if _, ok := w[p]; ok && rng.Intn(2) == 0 {
				delete(w, p)
				break
			}
			for k := range w { // keep each writer's tree prefix-free
				if strings.HasPrefix(k, p+"/") || strings.HasPrefix(p, k+"/") {
					delete(w, k)
				}
			}
			w[p] = item(fmt.Sprint(p, rng.Intn(2)))
		}
		var ws []Writer
		var ranked []map[string]tree.Entry
		for _, i := range order {
			if len(leaves[i]) == 0 && rng.Intn(4) == 0 {
				continue // a writer without a head
			}
			ws = append(ws, Writer{DID: dids[i], Root: buildCatalog(t, st, leaves[i])})
			ranked = append(ranked, leaves[i])
		}
		if err := u.Update(ws, drop); err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		want := reference(ranked, drop)
		if root := buildCatalog(t, st, want); root != u.Root {
			t.Fatalf("step %d: merged root %s, want %s", step, u.Root, root)
		}
		var size uint64
		for _, e := range want {
			size += e.Size
		}
		if u.Size != size {
			t.Fatalf("step %d: merged size %d, want %d", step, u.Size, size)
		}
		if len(view) != len(want) {
			t.Fatalf("step %d: stream has %d items, want %d", step, len(view), len(want))
		}
		full, _, err := Merge(st, ws, drop)
		if err != nil || full != u.Root {
			t.Fatalf("step %d: one-shot merge %s (%v), incremental %s", step, full, err, u.Root)
		}
	}
}

func TestUnionConflicts(t *testing.T) {
	st := mst.MemStore{}
	a := buildCatalog(t, st, map[string]tree.Entry{"2026/x": item("x1"), "2026/y": item("y"), "z": item("z")})
	b := buildCatalog(t, st, map[string]tree.Entry{"2026/x": item("x2"), "2026/y": item("y"), "z/inner": item("i")})
	got := map[string][]Loser{}
	u := &Union{St: st, OnDecide: func(p string, _ *Leaf, l []Loser) error {
		if len(l) > 0 {
			got[p] = l
		}
		return nil
	}}
	if err := u.Update([]Writer{{"a", a}, {"b", b}}, nil); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["2026/x"][0].Reason != ReasonEntry || got["2026/x"][0].Writer != "b" || got["z/inner"][0].Reason != ReasonStructure {
		t.Fatalf("conflicts %+v", got)
	}
	if e, _ := tree.Resolve(st, u.Root, "2026/x"); e != item("x1") {
		t.Fatal("earlier writer did not win")
	}
}
