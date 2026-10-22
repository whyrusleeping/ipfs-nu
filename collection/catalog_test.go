package collection

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/store"
)

// TestDeltaCatalog: a writer head that adds and removes a few items of a large held
// catalog is checked and fetched object by object for its change only, also when its
// MST grew a level; a large change falls back to the whole-catalog walk.
func TestDeltaCatalog(t *testing.T) {
	dir := t.TempDir()
	open := func(name string) *store.Store {
		st, err := store.Open(filepath.Join(dir, name), store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return st
	}
	remote, local := open("remote"), open("local")
	leaf := func(i int) (string, tree.Entry) {
		mc, mb, _ := drisl.Sum(map[string]any{"stac": map[string]any{"id": fmt.Sprint("s", i)}})
		if err := remote.Put(mc, mb); err != nil {
			t.Fatal(err)
		}
		e := item(fmt.Sprint(i))
		e.Meta = mc
		return fmt.Sprintf("2026/10/%02d/s%05d", 10+i%5, i), e
	}
	head := func(lo, hi int, skip ...int) cid.CID {
		leaves := map[string]tree.Entry{}
		for i := lo; i < hi; i++ {
			p, e := leaf(i)
			leaves[p] = e
		}
		for _, i := range skip {
			p, _ := leaf(i)
			delete(leaves, p)
		}
		hc, hb, _ := drisl.Sum(tree.Header{V: 1, Root: buildCatalog(t, remote, leaves)})
		if err := remote.Put(hc, hb); err != nil {
			t.Fatal(err)
		}
		return hc
	}
	fetched := 0
	fetchCatalog := func(_ context.Context, root cid.CID, skip func(cid.CID) bool) error {
		stack := []cid.CID{root}
		for len(stack) > 0 {
			c := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			b, err := remote.Get(c)
			if err != nil {
				return err
			}
			if err := local.Put(c, b); err != nil {
				return err
			}
			fetched++
			kids, _ := tree.CatalogLinks(c, b)
			for _, k := range kids {
				if skip == nil || !skip(k) {
					stack = append(stack, k)
				}
			}
		}
		return nil
	}
	m := &Manager{cfg: Config{Store: local, FetchCatalog: fetchCatalog}}
	ctx := context.Background()
	complete := func(h cid.CID) bool { return m.heldCatalog(ctx, catalogPin(h, "")) }

	small := head(0, 10)
	if err := m.syncWriterCatalog(ctx, small, nil); err != nil || !complete(small) {
		t.Fatalf("full sync: %v", err)
	}
	if fetched == 0 {
		t.Fatal("nothing fetched")
	}
	for _, c := range []struct {
		name     string
		old, new cid.CID
		max      int
	}{
		{"doubles", head(0, 300), head(0, 600), 400},
		{"too large a change", small, head(0, 3000), -1},
		{"adds 5, removes 1", head(0, 3000), head(0, 3005, 17), 40},
	} {
		if err := m.syncWriterCatalog(ctx, c.old, nil); err != nil {
			t.Fatal(err)
		}
		fetched = 0
		ok, err := m.deltaCatalog(ctx, c.new, []cid.CID{c.old})
		if c.max < 0 {
			// More than maxDeltaFetch objects on a level: the caller fetches the whole.
			if err != nil || ok {
				t.Fatalf("%s: %v %v", c.name, ok, err)
			}
			if err := m.syncWriterCatalog(ctx, c.new, []cid.CID{c.old}); err != nil || !complete(c.new) {
				t.Fatalf("%s: fallback: %v", c.name, err)
			}
			continue
		}
		if err != nil || !ok || !complete(c.new) || fetched > c.max {
			t.Fatalf("%s: ok %v err %v, %d objects fetched", c.name, ok, err, fetched)
		}
		t.Logf("%s: %d objects fetched", c.name, fetched)
	}
}
