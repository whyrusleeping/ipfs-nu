package collection

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
)

// Facet types.
const (
	TypeTime   = "time"
	TypeNumber = "number"
	TypeString = "string"
	TypeBBox   = "bbox"
)

// Limits on facet declarations.
const (
	MaxFacets     = 32
	MaxPointerLen = 256
)

// FacetSpec declares one facet: a JSON Pointer into the metadata record's JSON form and a
// type.
type FacetSpec struct {
	Ptr  string `drisl:"ptr" json:"ptr" toml:"ptr"`
	Type string `drisl:"type" json:"type" toml:"type"`
}

var facetNameRE = regexp.MustCompile(`^[a-z][a-z0-9_:.-]{0,63}$`)

// reserved are rule keys that are not facet predicates.
var reserved = []string{"name", "hold", "path", "max_bytes", "files", "rescue", "note"}

// ValidateFacets checks facet declarations: at most MaxFacets names matching
// [a-z][a-z0-9_:.-]{0,63} that are not rule keywords, RFC 6901 pointers of at most 256 bytes,
// and known types.
func ValidateFacets(f map[string]FacetSpec) error {
	if len(f) > MaxFacets {
		return fmt.Errorf("collection: more than %d facets", MaxFacets)
	}
	for name, s := range f {
		if !facetNameRE.MatchString(name) || slices.Contains(reserved, name) {
			return fmt.Errorf("collection: invalid facet name %q", name)
		}
		if _, err := parsePointer(s.Ptr); err != nil {
			return fmt.Errorf("collection: facet %s: %w", name, err)
		}
		switch s.Type {
		case TypeTime, TypeNumber, TypeString, TypeBBox:
		default:
			return fmt.Errorf("collection: facet %s: unknown type %q", name, s.Type)
		}
	}
	return nil
}

// FacetsFromHeader reads the facet declarations of a tree header's `collection` map
// ({facets: {name: {ptr, type}}}).
func FacetsFromHeader(coll map[string]any) (map[string]FacetSpec, error) {
	raw, ok := coll["facets"].(map[string]any)
	if !ok {
		return nil, nil
	}
	out := map[string]FacetSpec{}
	for name, v := range raw {
		var s FacetSpec
		if err := drisl.Assign(v, &s); err != nil {
			return nil, fmt.Errorf("collection: facet %s: %w", name, err)
		}
		out[name] = s
	}
	return out, ValidateFacets(out)
}

func parsePointer(p string) ([]string, error) {
	if len(p) > MaxPointerLen {
		return nil, errors.New("pointer too long")
	}
	if p == "" {
		return nil, nil
	}
	if p[0] != '/' {
		return nil, fmt.Errorf("pointer %q does not start with /", p)
	}
	toks := strings.Split(p[1:], "/")
	for i, t := range toks {
		for j := 0; j < len(t); j++ {
			if t[j] == '~' && (j+1 == len(t) || (t[j+1] != '0' && t[j+1] != '1')) {
				return nil, fmt.Errorf("pointer %q has a bad ~ escape", p)
			}
		}
		toks[i] = strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
	}
	return toks, nil
}

// Value is an extracted facet value. Time is Unix milliseconds; BBox is [west, south,
// east, north] in degrees.
type Value struct {
	Type string
	Time int64
	Num  float64
	Str  string
	BBox [4]float64
}

// JSON renders the value in its JSON form: time as RFC 3339 UTC with milliseconds,
// numbers, strings and bboxes as themselves.
func (v Value) JSON() any {
	switch v.Type {
	case TypeTime:
		return time.UnixMilli(v.Time).UTC().Format("2006-01-02T15:04:05.000Z")
	case TypeNumber:
		return v.Num
	case TypeString:
		return v.Str
	case TypeBBox:
		return v.BBox[:]
	}
	return nil
}

// Extract evaluates every facet over a decoded metadata record (generic DRISL). A missing
// or ill-typed value leaves the facet absent.
func Extract(specs map[string]FacetSpec, meta any) map[string]Value {
	out := map[string]Value{}
	for name, s := range specs {
		toks, err := parsePointer(s.Ptr)
		if err != nil {
			continue
		}
		raw, ok := resolve(meta, toks)
		if !ok {
			continue
		}
		if v, ok := typed(s.Type, raw); ok {
			out[name] = v
		}
	}
	return out
}

// resolve follows pointer tokens over the atproto-style JSON form of v: links are
// {"$link": "<cid>"} and bytes {"$bytes": "<base64>"}.
func resolve(v any, toks []string) (any, bool) {
	for _, t := range toks {
		switch x := v.(type) {
		case map[string]any:
			el, ok := x[t]
			if !ok {
				return nil, false
			}
			v = el
		case []any:
			i, ok := arrayIndex(t, len(x))
			if !ok {
				return nil, false
			}
			v = x[i]
		case cid.CID:
			if t != "$link" {
				return nil, false
			}
			v = x.String()
		case drisl.ForeignLink:
			if t != "$link" {
				return nil, false
			}
			v = x.String()
		case []byte:
			if t != "$bytes" {
				return nil, false
			}
			v = base64.RawStdEncoding.EncodeToString(x)
		default:
			return nil, false
		}
	}
	return v, true
}

func arrayIndex(t string, n int) (int, bool) {
	if t == "" || (len(t) > 1 && t[0] == '0') {
		return 0, false
	}
	for _, c := range t {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	i, err := strconv.Atoi(t)
	return i, err == nil && i < n
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case drisl.NegInt:
		return -1 - float64(uint64(x)), true
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	}
	return 0, false
}

// ParseTime parses a time facet or bound: an RFC 3339 string (with time zone; fractions
// beyond milliseconds are truncated) or integer Unix milliseconds.
func ParseTime(v any) (int64, bool) {
	switch x := v.(type) {
	case string:
		t, err := time.Parse(time.RFC3339Nano, x)
		if err != nil {
			return 0, false
		}
		return t.UnixMilli(), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	case uint64:
		if x > math.MaxInt64 {
			return 0, false
		}
		return int64(x), true
	}
	return 0, false
}

// ParseBBox normalises a bbox: [west, south, east, north], or the 3D form [west, south,
// min-z, east, north, max-z] reduced to 2D. Longitudes must lie in [-180, 180], latitudes
// in [-90, 90] with south <= north; west > east crosses the antimeridian.
func ParseBBox(v any) ([4]float64, bool) {
	var b [4]float64
	arr, ok := v.([]any)
	if !ok || (len(arr) != 4 && len(arr) != 6) {
		return b, false
	}
	idx := []int{0, 1, 2, 3}
	if len(arr) == 6 {
		idx = []int{0, 1, 3, 4}
	}
	for i, j := range idx {
		f, ok := number(arr[j])
		if !ok {
			return b, false
		}
		b[i] = f
	}
	for _, j := range len6(len(arr)) {
		if _, ok := number(arr[j]); !ok {
			return b, false
		}
	}
	w, s, e, n := b[0], b[1], b[2], b[3]
	if w < -180 || w > 180 || e < -180 || e > 180 || s < -90 || n > 90 || s > n {
		return b, false
	}
	return b, true
}

func len6(n int) []int {
	if n == 6 {
		return []int{2, 5}
	}
	return nil
}

func typed(typ string, raw any) (Value, bool) {
	v := Value{Type: typ}
	var ok bool
	switch typ {
	case TypeTime:
		if _, isFloat := raw.(float64); isFloat {
			return v, false
		}
		v.Time, ok = ParseTime(raw)
	case TypeNumber:
		v.Num, ok = number(raw)
	case TypeString:
		v.Str, ok = raw.(string)
	case TypeBBox:
		v.BBox, ok = ParseBBox(raw)
	}
	return v, ok
}
