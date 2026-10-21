package collection

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
)

const vectorFile = "../testdata/vectors/nu-collection/vectors.json"

type facetCase struct {
	Name   string         `json:"name"`
	Meta   string         `json:"meta"`
	Expect map[string]any `json:"expect"`
}

type predCase struct {
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Value  any            `json:"value"`
	Pred   map[string]any `json:"pred"`
	Now    string         `json:"now"`
	Expect bool           `json:"expect"`
}

type mergeWriter struct {
	DID   string                `json:"did"`
	Items map[string]tree.Entry `json:"-"`
	JSON  map[string]entryJSON  `json:"items"`
}

type entryJSON struct {
	Kind string `json:"kind"`
	CID  string `json:"cid"`
	Size uint64 `json:"size"`
	Meta string `json:"meta,omitempty"`
}

type mergeCase struct {
	Name    string        `json:"name"`
	Writers []mergeWriter `json:"writers"`
	Drop    []string      `json:"drop,omitempty"`
	Root    string        `json:"root"`
	Size    uint64        `json:"size"`
	Blocks  int           `json:"blocks"`
}

type vectors struct {
	Note       string               `json:"note"`
	Facets     map[string]FacetSpec `json:"facets"`
	FacetCases []facetCase          `json:"facet_cases"`
	Predicates []predCase           `json:"predicates"`
	Merges     []mergeCase          `json:"merges"`
}

var stacFacets = map[string]FacetSpec{
	"sensed":   {Ptr: "/stac/properties/datetime", Type: TypeTime},
	"bbox":     {Ptr: "/stac/bbox", Type: TypeBBox},
	"cloud":    {Ptr: "/stac/properties/eo:cloud_cover", Type: TypeNumber},
	"platform": {Ptr: "/stac/properties/platform", Type: TypeString},
	"link":     {Ptr: "/preview/$link", Type: TypeString},
	"odd":      {Ptr: "/a~1b/~0c", Type: TypeString},
}

func buildVectors(t *testing.T) vectors {
	v := vectors{Note: "D22 collection vectors (docs/collections.md): facet extraction over the JSON form of metadata, predicate outcomes, union-merged catalog roots", Facets: stacFacets}
	metas := []struct{ name, meta string }{
		{"stac", `{"stac":{"bbox":[5.9,45.8,10.5,47.8],"properties":{"datetime":"2026-10-19T10:30:00.123456Z","eo:cloud_cover":12.5,"platform":"sentinel-2b"}},"preview":{"$link":"bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"},"a/b":{"~c":"escaped"}}`},
		{"integer-time-3d-bbox", `{"stac":{"bbox":[170,-10,0,-170,10,100],"properties":{"datetime":1760869800000,"eo:cloud_cover":3,"platform":"landsat-9"}}}`},
		{"ill-typed", `{"stac":{"bbox":[5.9,95,10.5,47.8],"properties":{"datetime":"19 Oct 2026","eo:cloud_cover":"low","platform":7}}}`},
		{"float-time-swapped-lat", `{"stac":{"bbox":[0,10,1,5],"properties":{"datetime":1760869800000.5}}}`},
		{"missing", `{"other":true}`},
	}
	for _, m := range metas {
		val, err := drisl.FromJSON([]byte(m.meta), drisl.JSONOptions{})
		if err != nil {
			t.Fatal(err)
		}
		exp := map[string]any{}
		for k, fv := range Extract(stacFacets, val) {
			exp[k] = fv.JSON()
		}
		b, _ := json.Marshal(exp)
		_ = json.Unmarshal(b, &exp)
		v.FacetCases = append(v.FacetCases, facetCase{Name: m.name, Meta: m.meta, Expect: exp})
	}
	now := "2026-10-20T12:00:00Z"
	alps := []any{5.9, 45.8, 10.5, 47.8}
	preds := []predCase{
		{Name: "eq", Type: TypeString, Value: "sentinel-2b", Pred: map[string]any{"eq": "sentinel-2b"}},
		{Name: "eq-mismatch", Type: TypeString, Value: "sentinel-2a", Pred: map[string]any{"eq": "sentinel-2b"}},
		{Name: "in", Type: TypeString, Value: "sentinel-2a", Pred: map[string]any{"in": []any{"sentinel-2a", "sentinel-2b"}}},
		{Name: "max-inclusive", Type: TypeNumber, Value: 30.0, Pred: map[string]any{"max": 30.0}},
		{Name: "max-exceeded", Type: TypeNumber, Value: 30.5, Pred: map[string]any{"max": 30.0}},
		{Name: "min-max", Type: TypeNumber, Value: 5.0, Pred: map[string]any{"min": 0.0, "max": 10.0}},
		{Name: "time-min-rfc3339", Type: TypeTime, Value: "2026-10-19T00:00:00Z", Pred: map[string]any{"min": "2026-10-19T00:00:00Z"}},
		{Name: "time-max-ms", Type: TypeTime, Value: "2026-10-19T00:00:00.001Z", Pred: map[string]any{"max": 1760832000000.0}},
		{Name: "within-inside", Type: TypeTime, Value: "2026-10-18T12:00:00Z", Pred: map[string]any{"within": "48h"}},
		{Name: "within-edge", Type: TypeTime, Value: "2026-10-18T12:00:00Z", Pred: map[string]any{"within": "2d"}},
		{Name: "within-outside", Type: TypeTime, Value: "2026-10-18T11:59:59.999Z", Pred: map[string]any{"within": "2d"}},
		{Name: "within-future", Type: TypeTime, Value: "2026-10-21T00:00:00Z", Pred: map[string]any{"within": "6h"}},
		{Name: "bbox-overlap", Type: TypeBBox, Value: []any{10.0, 47.0, 12.0, 49.0}, Pred: map[string]any{"intersects": alps}},
		{Name: "bbox-touch", Type: TypeBBox, Value: []any{10.5, 40.0, 12.0, 45.8}, Pred: map[string]any{"intersects": alps}},
		{Name: "bbox-disjoint", Type: TypeBBox, Value: []any{-10.0, 40.0, 0.0, 45.0}, Pred: map[string]any{"intersects": alps}},
		{Name: "bbox-antimeridian", Type: TypeBBox, Value: []any{175.0, -5.0, -175.0, 5.0}, Pred: map[string]any{"intersects": []any{-178.0, 0.0, -170.0, 1.0}}},
		{Name: "bbox-antimeridian-miss", Type: TypeBBox, Value: []any{175.0, -5.0, -175.0, 5.0}, Pred: map[string]any{"intersects": []any{0.0, 0.0, 10.0, 1.0}}},
		{Name: "absent", Type: TypeNumber, Value: nil, Pred: map[string]any{"max": 30.0}},
	}
	for _, p := range preds {
		p.Now = now
		p.Expect = evalCase(t, p)
		v.Predicates = append(v.Predicates, p)
	}
	st := mst.MemStore{}
	ent := func(s string, withMeta bool) entryJSON {
		e := item(s)
		j := entryJSON{Kind: e.Kind, CID: e.CID.String(), Size: e.Size}
		if withMeta {
			j.Meta = e.Meta.String()
		}
		return j
	}
	cases := []mergeCase{
		{Name: "two-writers-overlap", Writers: []mergeWriter{
			{DID: "did:key:writer-a", JSON: map[string]entryJSON{"2026/10/19/s2a": ent("s2a", true), "2026/10/19/s2b": ent("s2b", true)}},
			{DID: "did:key:writer-b", JSON: map[string]entryJSON{"2026/10/19/s2b": ent("s2b", true), "2026/10/20/s2c": ent("s2c", true)}}}},
		{Name: "conflict-earlier-writer-wins", Writers: []mergeWriter{
			{DID: "did:key:writer-a", JSON: map[string]entryJSON{"2026/x": ent("x-a", true)}},
			{DID: "did:key:writer-b", JSON: map[string]entryJSON{"2026/x": ent("x-b", true), "2026/y": ent("y", true)}}}},
		{Name: "structure-and-drop", Drop: []string{"2026/y"}, Writers: []mergeWriter{
			{DID: "did:key:writer-a", JSON: map[string]entryJSON{"2026/x": ent("x", true), "readme": ent("readme", false)}},
			{DID: "did:key:writer-b", JSON: map[string]entryJSON{"2026/x/inner": ent("inner", true), "2026/y": ent("y", true)}}}},
		{Name: "empty", Writers: []mergeWriter{}},
	}
	for _, c := range cases {
		var ws []Writer
		for i := range c.Writers {
			ws = append(ws, Writer{DID: c.Writers[i].DID, Root: buildCatalog(t, st, decodeItems(t, c.Writers[i].JSON))})
		}
		ms := mst.MemStore{}
		cp := copyStore{st, ms}
		root, size, err := Merge(cp, ws, c.Drop)
		if err != nil {
			t.Fatal(err)
		}
		c.Root, c.Size, c.Blocks = root.String(), size, len(ms)
		v.Merges = append(v.Merges, c)
	}
	return v
}

// copyStore reads from base and records new blocks in out.
type copyStore struct {
	base mst.MemStore
	out  mst.MemStore
}

func (c copyStore) Get(k cid.CID) ([]byte, error) { return c.base.Get(k) }
func (c copyStore) Put(k cid.CID, b []byte) error {
	if _, ok := c.base[k]; !ok {
		c.out[k] = b
	}
	return c.base.Put(k, b)
}

func decodeItems(t *testing.T, in map[string]entryJSON) map[string]tree.Entry {
	out := map[string]tree.Entry{}
	for p, j := range in {
		e := tree.Entry{Kind: j.Kind, CID: cid.MustParse(j.CID), Size: j.Size}
		if j.Meta != "" {
			e.Meta = cid.MustParse(j.Meta)
		}
		out[p] = e
	}
	return out
}

func evalCase(t *testing.T, p predCase) bool {
	t.Helper()
	pred, err := parsePred(p.Pred)
	if err != nil {
		t.Fatalf("%s: %v", p.Name, err)
	}
	if err := pred.check(p.Type); err != nil {
		t.Fatalf("%s: %v", p.Name, err)
	}
	var val Value
	ok := p.Value != nil
	if ok {
		val, ok = typed(p.Type, normNums(p.Value))
		if !ok {
			t.Fatalf("%s: bad value %v", p.Name, p.Value)
		}
	}
	now, _ := time.Parse(time.RFC3339, p.Now)
	return pred.Eval(val, ok, now)
}

func TestVectors(t *testing.T) {
	got := buildVectors(t)
	if os.Getenv("NU_UPDATE_VECTORS") == "1" {
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.MkdirAll(filepath.Dir(vectorFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(vectorFile, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(vectorFile)
	if err != nil {
		t.Fatal(err)
	}
	var want vectors
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(got)
	var g vectors
	_ = json.Unmarshal(gb, &g)
	if !reflect.DeepEqual(g, want) {
		t.Fatal("collection vectors changed; they are a permanent contract (NU_UPDATE_VECTORS=1 only for new vectors)")
	}
	// The file alone reproduces every outcome.
	for _, p := range want.Predicates {
		if evalCase(t, p) != p.Expect {
			t.Errorf("predicate %s", p.Name)
		}
	}
	for _, c := range want.Merges {
		st := mst.MemStore{}
		var ws []Writer
		for _, w := range c.Writers {
			ws = append(ws, Writer{DID: w.DID, Root: buildCatalog(t, st, decodeItems(t, w.JSON))})
		}
		root, size, err := Merge(st, ws, c.Drop)
		if err != nil || root.String() != c.Root || size != c.Size {
			t.Errorf("merge %s: %s %d %v", c.Name, root, size, err)
		}
	}
}

func TestRulesTOML(t *testing.T) {
	var doc struct {
		Rule []map[string]any `toml:"rule"`
	}
	src := `
[[rule]]        # alpine scenes, mostly clear, for 48 h
bbox   = { intersects = [5.9, 45.8, 10.5, 47.8] }
cloud  = { max = 30 }
sensed = { within = "48h" }

[[rule]]
platform = { eq = "landsat-9" }
hold = false

[[rule]]        # spare capacity for under-replicated scenes
rescue = { below = 2, max_bytes = "100GiB" }
`
	if _, err := toml.Decode(src, &doc); err != nil {
		t.Fatal(err)
	}
	var rules []Rule
	for _, m := range doc.Rule {
		r, err := ParseRule(m)
		if err != nil {
			t.Fatal(err)
		}
		rules = append(rules, r)
	}
	if err := Compile(rules, stacFacets); err != nil {
		t.Fatal(err)
	}
	if rules[2].Budget() != 100<<30 || rules[2].Rescue.Below != 2 || rules[1].Holds() {
		t.Fatalf("parsed %+v", rules)
	}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	meta, _ := drisl.FromJSON([]byte(`{"stac":{"bbox":[6,46,7,47],"properties":{"datetime":"2026-10-20T00:00:00Z","eo:cloud_cover":10,"platform":"landsat-9"}}}`), drisl.JSONOptions{})
	f := Extract(stacFacets, meta)
	if got := Select(rules, "2026/a", f, now); got != 0 {
		t.Fatalf("first match %d, want 0", got)
	}
	if got := Select(rules, "2026/a", f, now.Add(72*time.Hour)); got != 1 {
		t.Fatalf("after the window: match %d, want 1 (hold = false)", got)
	}
	// JSON round trip keeps the flat form.
	b, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	var back []Rule
	if err := json.Unmarshal(b, &back); err != nil || Compile(back, stacFacets) != nil || Select(back, "2026/a", f, now) != 0 {
		t.Fatalf("round trip %s: %v", b, err)
	}
	if err := Compile([]Rule{{Preds: map[string]Pred{"cloud": {Within: "1h", within: time.Hour}}}}, stacFacets); err == nil {
		t.Fatal("within on a number facet accepted")
	}
}
