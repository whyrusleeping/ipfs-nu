package drisl

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/whyrusleeping/nu/core/cid"
)

const vectors = "../../testdata/vectors"

type daslCase struct {
	Type, Data, Name string
	Tags             []string
}

// daslApplies selects the dasl-testing cases for DRISL; other tags describe other profiles.
func daslApplies(tags []string) bool {
	for _, t := range tags {
		if t == "dag-cbor" || t == "basic" || t == "dasl-cid" {
			return true
		}
	}
	return false
}

func TestDASLTestingVectors(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(vectors, "dasl-testing", "*.json"))
	if len(files) == 0 {
		t.Fatal("no vectors")
	}
	n := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var cases []daslCase
		if err := json.Unmarshal(raw, &cases); err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			if !daslApplies(c.Tags) {
				continue
			}
			n++
			in, _ := hex.DecodeString(c.Data)
			v, err := Decode(in)
			switch c.Type {
			case "roundtrip":
				if err != nil {
					t.Errorf("%s/%s: decode: %v", filepath.Base(f), c.Name, err)
					continue
				}
				out, err := Marshal(v)
				if err != nil || !bytes.Equal(out, in) {
					t.Errorf("%s/%s: re-encode %x, %v", filepath.Base(f), c.Name, out, err)
				}
			case "invalid_in", "invalid_out":
				// invalid_out inputs are generic CBOR that DRISL cannot represent, so the strict
				// decoder must refuse them too; encoder-side checks are in TestEncodeRejects.
				if err == nil {
					t.Errorf("%s/%s: accepted %x", filepath.Base(f), c.Name, in)
				}
			}
		}
	}
	t.Logf("%d dasl-testing cases", n)
}

func TestEncodeRejects(t *testing.T) {
	for _, v := range []any{math.NaN(), math.Inf(1), math.Inf(-1), math.Copysign(0, -1), "\xff", cid.CID{}, map[int]any{1: 1}} {
		if b, err := Marshal(v); err == nil {
			t.Errorf("Marshal(%#v) = %x", v, b)
		}
	}
}

func TestAtprotoFixtures(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(vectors, "atproto-data-model", "data-model-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx []struct {
		JSON       json.RawMessage `json:"json"`
		CBORBase64 string          `json:"cbor_base64"`
		CID        string          `json:"cid"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	for i, f := range fx {
		want, _ := base64.RawStdEncoding.DecodeString(f.CBORBase64)
		v, err := FromJSON(f.JSON, JSONOptions{Atproto: true})
		if err != nil {
			t.Fatalf("%d: FromJSON: %v", i, err)
		}
		c, enc, err := Sum(v)
		if err != nil || !bytes.Equal(enc, want) || c.String() != f.CID {
			t.Fatalf("%d: encode %x cid %s (%v)", i, enc, c, err)
		}
		back, err := AtprotoRecord.Decode(want)
		if err != nil || !reflect.DeepEqual(back, v) || ValidateAtproto(back) != nil {
			t.Fatalf("%d: decode: %v", i, err)
		}
		js, err := ToJSON(back)
		if err != nil {
			t.Fatal(err)
		}
		if !jsonEqual(t, js, f.JSON) {
			t.Fatalf("%d: JSON %s, want %s", i, js, f.JSON)
		}
	}
	for _, name := range []string{"valid", "invalid"} {
		raw, err := os.ReadFile(filepath.Join(vectors, "atproto-data-model", "data-model-"+name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var cases []struct {
			Note string          `json:"note"`
			JSON json.RawMessage `json:"json"`
		}
		if err := json.Unmarshal(raw, &cases); err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			v, err := FromJSON(c.JSON, JSONOptions{Atproto: true})
			if err == nil {
				err = ValidateAtproto(v)
			}
			if (err == nil) != (name == "valid") {
				t.Errorf("%s %q: err = %v", name, c.Note, err)
			}
		}
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		t.Fatal("bad json")
	}
	return reflect.DeepEqual(x, y)
}

// Non-canonical encodings beyond the dasl-testing suite.
func TestRejectNonCanonical(t *testing.T) {
	link := "d82a58250001711220" + "65062a5a5a00fc16d73c6944237ccbc15b1c4a7234489336891d091741a239d0"
	for name, h := range map[string]string{
		"key order length-first": "a26262620161610" + "2",
		"bytewise order":         "a2616201616102",
		"non-string key":         "a1f6f6",
		"int key":                "a10101",
		"float32":                "fa3fc00000",
		"float16":                "f93c00",
		"undefined":              "f7",
		"simple 24":              "f818",
		"tag 0":                  "c06161",
		"tag 42 of text":         "d82a6161",
		"tag 42 no prefix":       "d82a58240171122065062a5a5a00fc16d73c6944237ccbc15b1c4a7234489336891d091741a239d0",
		"trailing bytes":         link + "00",
		"truncated":              link[:len(link)-2],
		"array len overflow":     "9b00ffffffffffffff",
		"break":                  "ff",
		"non-minimal len 24":     "781761616161616161616161616161616161616161616161",
	} {
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := Decode(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Decode(mustHex(link)); err != nil {
		t.Fatalf("valid link rejected: %v", err)
	}
	// Nesting limit.
	deep := bytes.Repeat([]byte{0x81}, DefaultMaxDepth+1)
	if _, err := Decode(append(deep, 0)); err != ErrTooDeep {
		t.Fatalf("deep: %v", err)
	}
	// Atproto profile: no floats, int64 only; foreign links only when enabled.
	if _, err := AtprotoRecord.Decode(mustHex("fb3ff8000000000000")); err == nil {
		t.Error("atproto float accepted")
	}
	if _, err := AtprotoRecord.Decode(mustHex("1bffffffffffffffff")); err == nil {
		t.Error("atproto uint64 accepted")
	}
	dagpb := "d82a58250001701220e9822efc7c48027a5429fdbd988d02b2b8e4eaee8f62c32bd1021dcf922e05de"
	if _, err := Decode(mustHex(dagpb)); err == nil {
		t.Error("foreign link accepted in strict mode")
	}
	v, err := AtprotoRecord.Decode(mustHex(dagpb))
	if _, ok := v.(ForeignLink); !ok || err != nil {
		t.Fatalf("foreign link: %v %v", v, err)
	}
	if out, _ := Marshal(v); !bytes.Equal(out, mustHex(dagpb)) {
		t.Fatal("foreign link not byte-exact")
	}
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func randValue(r *rand.Rand, depth int) any {
	k := r.IntN(12)
	if depth > 3 && k >= 9 {
		k = r.IntN(9)
	}
	switch k {
	case 0:
		return nil
	case 1:
		return r.IntN(2) == 0
	case 2:
		return int64(r.Uint64()) >> r.IntN(64)
	case 3:
		return r.Uint64() | 1<<63
	case 4:
		return NegInt(r.Uint64() | 1<<63)
	case 5:
		f := math.Float64frombits(r.Uint64())
		if checkFloat(f) != nil {
			return 1.5
		}
		return f
	case 6:
		return string(randBytes(r, true))
	case 7:
		return randBytes(r, false)
	case 8:
		return cid.New(cid.Codec([]byte{0x55, 0x71}[r.IntN(2)]), cid.Sum(cid.Hash([]byte{0x12, 0x1e}[r.IntN(2)]), randBytes(r, false)))
	case 9, 10:
		a := make([]any, r.IntN(5))
		for i := range a {
			a[i] = randValue(r, depth+1)
		}
		return a
	default:
		m := map[string]any{}
		for range r.IntN(6) {
			m[string(randBytes(r, true))] = randValue(r, depth+1)
		}
		return m
	}
}

func randBytes(r *rand.Rand, text bool) []byte {
	b := make([]byte, r.IntN(6))
	for i := range b {
		if text {
			b[i] = byte('a' + r.IntN(4))
		} else {
			b[i] = byte(r.Uint32())
		}
	}
	return b
}

func TestRoundTripProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := range 500 {
		v := randValue(r, 0)
		b, err := Marshal(v)
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		back, err := Decode(b)
		if err != nil {
			t.Fatalf("%d: decode %x: %v", i, b, err)
		}
		b2, _ := Marshal(back)
		if !bytes.Equal(b, b2) || !reflect.DeepEqual(norm(v), back) {
			t.Fatalf("%d: round trip mismatch %x", i, b)
		}
		js, err := ToJSON(back)
		if err != nil {
			t.Fatal(err)
		}
		jv, err := FromJSON(js, JSONOptions{})
		if err != nil {
			t.Fatalf("%d: %s: %v", i, js, err)
		}
		if b3, _ := Marshal(jv); !bytes.Equal(b, b3) {
			t.Fatalf("%d: JSON round trip %s", i, js)
		}
	}
}

// norm maps the generator's empty byte slices to what decoding yields.
func norm(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = norm(x[i])
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = norm(e)
		}
		return out
	case []byte:
		return append([]byte{}, x...)
	}
	return v
}

type testEntry struct {
	Kind string   `drisl:"kind"`
	CID  cid.CID  `drisl:"cid"`
	Size uint64   `drisl:"size"`
	Mode *uint32  `drisl:"mode,omitempty"`
	Tags []string `drisl:"tags,omitempty"`
	Skip string   `drisl:"-"`
}

type testHeader struct {
	V    int64                `drisl:"v"`
	Ents map[string]testEntry `drisl:"ents"`
}

func (testHeader) DRISLType() string { return "io.example.test" }

func TestStructMapping(t *testing.T) {
	mode := uint32(0o755)
	h := testHeader{V: 1, Ents: map[string]testEntry{
		"a": {Kind: "file", CID: cid.New(cid.Raw, cid.Sum(cid.BLAKE3, nil)), Size: 3, Mode: &mode, Skip: "x"},
	}}
	b, err := Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := Decode(b)
	m := g.(map[string]any)
	if m["$type"] != "io.example.test" || slices.Collect(mapKeys(m["ents"].(map[string]any)["a"].(map[string]any))) == nil {
		t.Fatalf("%v", g)
	}
	var back testHeader
	if err := Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	h.Ents["a"] = testEntry{Kind: "file", CID: h.Ents["a"].CID, Size: 3, Mode: &mode}
	if !reflect.DeepEqual(h, back) {
		t.Fatalf("%+v != %+v", back, h)
	}
	m["$type"] = "other"
	b2, _ := Marshal(m)
	if Unmarshal(b2, &back) == nil {
		t.Fatal("wrong $type accepted")
	}
}

func mapKeys(m map[string]any) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func FuzzDecode(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join(vectors, "dasl-testing", "*.json"))
	for _, fn := range files {
		raw, _ := os.ReadFile(fn)
		var cases []daslCase
		_ = json.Unmarshal(raw, &cases)
		for _, c := range cases {
			f.Add(mustHex(c.Data))
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, o := range []DecOptions{{}, AtprotoRecord} {
			v, err := o.Decode(b)
			if err != nil {
				continue
			}
			out, err := Marshal(v)
			if err != nil || !bytes.Equal(out, b) {
				t.Fatalf("accepted %x but re-encodes to %x (%v)", b, out, err)
			}
			if _, err := ToJSON(v); err != nil {
				t.Fatal(err)
			}
		}
	})
}
