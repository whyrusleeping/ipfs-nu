package atrepo

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/mst"
)

const vecDir = "../../testdata/vectors/"

func readJSON(t testing.TB, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(vecDir + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

var leaf = cid.MustParse("bafyreie5cvv4h45feadgeuwhbcutmh6t2ceseocckahdoe6uat64zmz454")

func flush(t testing.TB, tr *mst.Tree[cid.CID]) cid.CID {
	t.Helper()
	c, err := tr.Flush()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// atproto-interop-tests MST vectors plus the reference root-CID cases.
func TestMSTVectors(t *testing.T) {
	var heights []struct {
		Key    string
		Height int
	}
	readJSON(t, "atproto-mst/key_heights.json", &heights)
	for _, h := range heights {
		if got := MST.Layer(h.Key); got != h.Height {
			t.Errorf("layer(%q) = %d, want %d", h.Key, got, h.Height)
		}
	}
	keys, err := os.ReadFile(vecDir + "atproto-mst/example_keys.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range strings.Fields(string(keys)) { // "<letter><layer>/<n>"
		if got := MST.Layer(k); got != int(k[1]-'0') {
			t.Errorf("layer(%q) = %d", k, got)
		}
	}
	var prefixes []struct {
		Left, Right string
		Len         int
	}
	readJSON(t, "atproto-mst/common_prefix.json", &prefixes)
	for _, p := range prefixes {
		if got := commonPrefix(p.Left, p.Right); got != p.Len {
			t.Errorf("prefix(%q, %q) = %d, want %d", p.Left, p.Right, got, p.Len)
		}
	}

	var maps []struct {
		Comment string
		Keys    []string
		Root    string
		Then    []struct{ Put, Del, Root string }
	}
	readJSON(t, "atproto-mst/known-maps.json", &maps)
	for _, m := range maps {
		st := mst.MemStore{}
		tr := LoadTree(st, cid.CID{})
		for _, k := range m.Keys {
			if err := tr.Put(k, leaf); err != nil {
				t.Fatal(err)
			}
		}
		if got := flush(t, tr); got.String() != m.Root {
			t.Errorf("%s: root %s, want %s", m.Comment, got, m.Root)
			continue
		}
		for i, s := range m.Then {
			// Alternate between mutating in memory and reloading from the store, so both
			// the in-memory and the decoded (intermediate-node) paths are exercised.
			if i%2 == 1 {
				tr = LoadTree(st, flush(t, tr))
			}
			if s.Put != "" {
				err = tr.Put(s.Put, leaf)
			} else {
				_, err = tr.Delete(s.Del)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := flush(t, tr)
			if got.String() != s.Root {
				t.Errorf("%s: step %d: root %s, want %s", m.Comment, i, got, s.Root)
			}
			// The flushed tree reloads and validates.
			if err := LoadTree(st, got).Walk("", func(string, cid.CID) (bool, error) { return true, nil }); err != nil {
				t.Errorf("%s: step %d: reload: %v", m.Comment, i, err)
			}
		}
	}
}
