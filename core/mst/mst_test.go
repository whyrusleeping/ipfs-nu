package mst

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/whyrusleeping/nu/core/cid"
)

var testProfile = &Profile[int64]{
	Name: "test", LevelBits: 4, MaxNodeSize: 1 << 20,
	Codec: InlineCodec[int64]{}, Equal: func(a, b int64) bool { return a == b },
}

func build(t testing.TB, st Store, keys []string, del []string) (*Tree[int64], cid.CID) {
	tr := testProfile.Load(st, cid.CID{})
	for _, k := range keys {
		if err := tr.Put(k, int64(len(k))); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range del {
		if ok, err := tr.Delete(k); err != nil || !ok {
			t.Fatalf("delete %q: %v %v", k, ok, err)
		}
	}
	c, err := tr.Flush()
	if err != nil {
		t.Fatal(err)
	}
	return tr, c
}

// Random insert/delete orders (with intermediate flushes and reloads) give identical roots.
func TestOrderIndependence(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 7))
	for iter := range 40 {
		n := 1 + r.IntN(300)
		keys := make([]string, n)
		for i := range keys {
			keys[i] = fmt.Sprintf("k%d-%d", iter, r.IntN(100000))
		}
		slices.Sort(keys)
		keys = slices.Compact(keys)
		extra := []string{}
		for i := range r.IntN(50) {
			extra = append(extra, fmt.Sprintf("x%d", i))
		}
		st := MemStore{}
		_, want := build(t, st, keys, nil)
		for range 3 {
			perm := append(slices.Clone(keys), extra...)
			r.Shuffle(len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
			tr := testProfile.Load(st, cid.CID{})
			for i, k := range perm {
				if err := tr.Put(k, int64(len(k))); err != nil {
					t.Fatal(err)
				}
				if i%37 == 0 { // reload from the store midway
					c, err := tr.Flush()
					if err != nil {
						t.Fatal(err)
					}
					tr = testProfile.Load(st, c)
				}
			}
			ex := slices.Clone(extra)
			r.Shuffle(len(ex), func(i, j int) { ex[i], ex[j] = ex[j], ex[i] })
			for _, k := range ex {
				if ok, err := tr.Delete(k); !ok || err != nil {
					t.Fatalf("delete %q", k)
				}
			}
			if got, _ := tr.Flush(); got != want {
				t.Fatalf("iter %d: root %s, want %s", iter, got, want)
			}
		}
		// Contents and order are intact.
		var got []string
		if err := testProfile.Load(st, want).Walk("", func(k string, v int64) (bool, error) {
			got = append(got, k)
			return true, nil
		}); err != nil || !slices.Equal(got, keys) {
			t.Fatalf("walk: %v", err)
		}
	}
	// Deleting everything returns the empty root.
	st := MemStore{}
	_, empty := build(t, st, nil, nil)
	if _, c := build(t, st, []string{"a", "b", "c"}, []string{"b", "a", "c"}); c != empty {
		t.Fatal("empty tree root differs")
	}
}

func TestDiffAndProofs(t *testing.T) {
	st := MemStore{}
	var keys []string
	for i := range 2000 {
		keys = append(keys, fmt.Sprintf("file%05d", i))
	}
	a, ca := build(t, st, keys, nil)
	b := testProfile.Load(st, ca)
	_ = b.Put("file00100", 99)   // change
	_ = b.Put("new", 3)          // add
	_, _ = b.Delete("file01999") // remove
	cb, _ := b.Flush()

	counting := &countStore{Store: st}
	var changes []string
	err := Diff(testProfile.Load(counting, ca), testProfile.Load(counting, cb), func(c Change[int64]) error {
		changes = append(changes, fmt.Sprintf("%s:%v:%v", c.Key, c.Old != nil, c.New != nil))
		return nil
	})
	if err != nil || !slices.Equal(changes, []string{"file00100:true:true", "file01999:true:false", "new:false:true"}) {
		t.Fatalf("diff %v %v", changes, err)
	}
	if counting.gets > 60 {
		t.Fatalf("diff loaded %d nodes; shared subtrees not skipped", counting.gets)
	}

	for _, k := range []string{"file00042", "absent"} {
		proof, err := a.Prove(k)
		if err != nil {
			t.Fatal(err)
		}
		v, found, err := testProfile.VerifyProof(ca, k, proof)
		if err != nil || found != (k != "absent") || (found && v != int64(len(k))) {
			t.Fatalf("proof %q: %v %v %v", k, v, found, err)
		}
		if len(proof) > 1 {
			if _, _, err := testProfile.VerifyProof(ca, k, proof[:len(proof)-1]); err == nil {
				t.Fatalf("truncated proof for %q accepted", k)
			}
		}
	}
	var pre []string
	_ = a.WalkPrefix("file0199", func(k string, _ int64) (bool, error) { pre = append(pre, k); return true, nil })
	if len(pre) != 10 || pre[0] != "file01990" {
		t.Fatalf("prefix %v", pre)
	}
}

type countStore struct {
	Store
	gets int
}

func (c *countStore) Get(id cid.CID) ([]byte, error) {
	c.gets++
	return c.Store.Get(id)
}

func FuzzNodeDecode(f *testing.F) {
	st := MemStore{}
	var keys []string
	for i := range 300 {
		keys = append(keys, fmt.Sprint(i))
	}
	build(f, st, keys, nil)
	for _, b := range st {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		n, err := testProfile.Codec.Decode(b)
		if err != nil {
			return
		}
		c, err := cid.SumCID(cid.DRISL, cid.SHA256, b)
		if err != nil {
			return
		}
		// Loading as a root applies the structural checks; it must not panic.
		tr := testProfile.Load(MemStore{c: b}, c)
		_, _, _ = tr.Get("5")
		_ = n
	})
}
