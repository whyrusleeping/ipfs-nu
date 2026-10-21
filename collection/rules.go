package collection

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Pred is the predicate on one facet. All set fields must hold: eq/in for strings,
// min/max (inclusive) for numbers and times, within (a duration back from now; the facet
// time must be >= now - within) for times, intersects for bboxes (closed boxes; a box with
// west > east crosses the antimeridian). A predicate on an absent facet is false.
type Pred struct {
	Eq         *string     `json:"eq,omitempty"`
	In         []string    `json:"in,omitempty"`
	Min        any         `json:"min,omitempty"`
	Max        any         `json:"max,omitempty"`
	Within     string      `json:"within,omitempty"`
	Intersects *[4]float64 `json:"intersects,omitempty"`

	// compiled bounds
	minNum, maxNum         float64
	minMS, maxMS           int64
	hasMin, hasMax         bool
	minIsTime, maxIsTime   bool
	within                 time.Duration
}

// Rescue makes a rule hold the items it matches whose fresh complete-holder count is below
// Below, rarest first, within MaxBytes.
type Rescue struct {
	Below    int64 `json:"below"`
	MaxBytes int64 `json:"max_bytes,omitempty"`
}

// Rule is one selection rule. Rules are evaluated in order and the first match decides.
// Its JSON (and TOML) form is flat: the keys name, hold, path, max_bytes and rescue, and
// one predicate object per facet name.
type Rule struct {
	Name     string
	Hold     *bool  // default true; false excludes the items it matches
	Path     string // only items at or below this path
	MaxBytes int64  // logical bytes this rule may hold; 0 is unlimited
	Rescue   *Rescue
	Preds    map[string]Pred
}

// Holds reports whether a match holds the item.
func (r *Rule) Holds() bool { return r.Hold == nil || *r.Hold }

// Budget is the rule's byte budget (rescue.max_bytes, else max_bytes; 0: unlimited).
func (r *Rule) Budget() int64 {
	if r.Rescue != nil && r.Rescue.MaxBytes > 0 {
		return r.Rescue.MaxBytes
	}
	return r.MaxBytes
}

// MarshalJSON writes the flat form.
func (r Rule) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	for k, p := range r.Preds {
		m[k] = p
	}
	if r.Name != "" {
		m["name"] = r.Name
	}
	if r.Hold != nil {
		m["hold"] = *r.Hold
	}
	if r.Path != "" {
		m["path"] = r.Path
	}
	if r.MaxBytes != 0 {
		m["max_bytes"] = r.MaxBytes
	}
	if r.Rescue != nil {
		m["rescue"] = r.Rescue
	}
	return json.Marshal(m)
}

// UnmarshalJSON reads the flat form (without facet checks; see Compile).
func (r *Rule) UnmarshalJSON(b []byte) error {
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return err
	}
	out, err := ParseRule(m)
	if err != nil {
		return err
	}
	*r = out
	return nil
}

// ParseRule reads a rule from its generic flat form (decoded JSON with json.Number, or
// TOML).
func ParseRule(m map[string]any) (Rule, error) {
	var r Rule
	r.Preds = map[string]Pred{}
	for k, v := range m {
		var err error
		switch k {
		case "name", "note":
			s, ok := v.(string)
			if !ok {
				return r, fmt.Errorf("%s must be a string", k)
			}
			if k == "name" {
				r.Name = s
			}
		case "hold":
			b, ok := v.(bool)
			if !ok {
				return r, errors.New("hold must be true or false")
			}
			r.Hold = &b
		case "path":
			s, ok := v.(string)
			if !ok {
				return r, errors.New("path must be a string")
			}
			r.Path = strings.Trim(s, "/")
		case "max_bytes":
			r.MaxBytes, err = ParseBytes(v)
		case "rescue":
			rm, ok := v.(map[string]any)
			if !ok {
				return r, errors.New("rescue must be a table {below, max_bytes?}")
			}
			r.Rescue = &Rescue{}
			for rk, rv := range rm {
				switch rk {
				case "below":
					f, ok := num(rv)
					if !ok || f < 1 || f != math.Trunc(f) || f > 1000 {
						return r, errors.New("rescue.below must be an integer from 1 to 1000")
					}
					r.Rescue.Below = int64(f)
				case "max_bytes":
					if r.Rescue.MaxBytes, err = ParseBytes(rv); err != nil {
						return r, fmt.Errorf("rescue.max_bytes: %w", err)
					}
				default:
					return r, fmt.Errorf("unknown rescue key %q", rk)
				}
			}
			if r.Rescue.Below == 0 {
				return r, errors.New("rescue needs below")
			}
		case "files":
			return r, errors.New("partial items (files) are not supported yet")
		default:
			pm, ok := v.(map[string]any)
			if !ok {
				return r, fmt.Errorf("facet predicate %s must be a table", k)
			}
			p, err := parsePred(pm)
			if err != nil {
				return r, fmt.Errorf("%s: %w", k, err)
			}
			r.Preds[k] = p
		}
		if err != nil {
			return r, fmt.Errorf("%s: %w", k, err)
		}
	}
	if r.Rescue != nil && !r.Holds() {
		return r, errors.New("a rescue rule cannot set hold = false")
	}
	return r, nil
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case int:
		return float64(x), true
	}
	return number(v)
}

func parsePred(m map[string]any) (Pred, error) {
	var p Pred
	for k, v := range m {
		switch k {
		case "eq":
			s, ok := v.(string)
			if !ok {
				return p, errors.New("eq must be a string")
			}
			p.Eq = &s
		case "in":
			arr, ok := v.([]any)
			if !ok {
				return p, errors.New("in must be a list of strings")
			}
			p.In = []string{}
			for _, el := range arr {
				s, ok := el.(string)
				if !ok {
					return p, errors.New("in must be a list of strings")
				}
				p.In = append(p.In, s)
			}
		case "min", "max":
			switch x := v.(type) {
			case time.Time: // TOML datetime
				v = x.Format(time.RFC3339Nano)
			case json.Number:
				if i, err := x.Int64(); err == nil {
					v = i
				} else if f, err := x.Float64(); err == nil {
					v = f
				}
			}
			switch v.(type) {
			case string, int64, float64:
			default:
				return p, fmt.Errorf("%s must be a number or an RFC 3339 time", k)
			}
			if k == "min" {
				p.Min = v
			} else {
				p.Max = v
			}
		case "within":
			s, ok := v.(string)
			if !ok {
				return p, errors.New("within must be a duration such as \"48h\" or \"7d\"")
			}
			if _, err := ParseDuration(s); err != nil {
				return p, err
			}
			p.Within = s
		case "intersects":
			b, ok := ParseBBox(normNums(v))
			if !ok {
				return p, errors.New("intersects must be [west, south, east, north] in degrees")
			}
			p.Intersects = &b
		default:
			return p, fmt.Errorf("unknown predicate %q", k)
		}
	}
	return p, p.compile()
}

// normNums converts json.Number and int elements of a list to float64.
func normNums(v any) any {
	arr, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]any, len(arr))
	for i, el := range arr {
		if f, ok := num(el); ok {
			out[i] = f
		} else {
			out[i] = el
		}
	}
	return out
}

func (p *Pred) compile() error {
	bound := func(v any) (f float64, ms int64, isTime, ok bool) {
		switch x := v.(type) {
		case string:
			ms, ok = ParseTime(x)
			return 0, ms, true, ok
		case int64:
			return float64(x), x, false, true
		case float64:
			return x, 0, false, !math.IsNaN(x) && !math.IsInf(x, 0)
		}
		return 0, 0, false, false
	}
	var ok bool
	if p.Min != nil {
		if p.minNum, p.minMS, p.minIsTime, ok = bound(p.Min); !ok {
			return fmt.Errorf("bad min %v", p.Min)
		}
		p.hasMin = true
	}
	if p.Max != nil {
		if p.maxNum, p.maxMS, p.maxIsTime, ok = bound(p.Max); !ok {
			return fmt.Errorf("bad max %v", p.Max)
		}
		p.hasMax = true
	}
	if p.Within != "" {
		d, err := ParseDuration(p.Within)
		if err != nil {
			return err
		}
		p.within = d
	}
	return nil
}

// check validates the predicate against a facet type.
func (p *Pred) check(typ string) error {
	str := p.Eq != nil || p.In != nil
	rng := p.hasMin || p.hasMax
	switch {
	case str && typ != TypeString:
		return fmt.Errorf("eq/in apply to string facets, not %s", typ)
	case rng && typ != TypeNumber && typ != TypeTime:
		return fmt.Errorf("min/max apply to number and time facets, not %s", typ)
	case (p.minIsTime || p.maxIsTime) && typ != TypeTime:
		return errors.New("an RFC 3339 bound applies only to time facets")
	case p.within != 0 && typ != TypeTime:
		return fmt.Errorf("within applies to time facets, not %s", typ)
	case p.Intersects != nil && typ != TypeBBox:
		return fmt.Errorf("intersects applies to bbox facets, not %s", typ)
	case typ == TypeTime && rng && ((p.hasMin && !p.minIsTime && p.minNum != math.Trunc(p.minNum)) || (p.hasMax && !p.maxIsTime && p.maxNum != math.Trunc(p.maxNum))):
		return errors.New("time bounds are RFC 3339 strings or integer milliseconds")
	}
	return nil
}

// Compile validates rules against the collection's facets (unknown facets and
// predicates that do not fit the facet type are errors).
func Compile(rules []Rule, facets map[string]FacetSpec) error {
	for i := range rules {
		for name, p := range rules[i].Preds {
			s, ok := facets[name]
			if !ok {
				return fmt.Errorf("rule %d: unknown facet %q", i+1, name)
			}
			if err := p.check(s.Type); err != nil {
				return fmt.Errorf("rule %d: %s: %w", i+1, name, err)
			}
		}
	}
	return nil
}

// Eval reports whether p holds for v at now.
func (p *Pred) Eval(v Value, ok bool, now time.Time) bool {
	if !ok {
		return false
	}
	if p.Eq != nil && (v.Type != TypeString || v.Str != *p.Eq) {
		return false
	}
	if p.In != nil && (v.Type != TypeString || !slices.Contains(p.In, v.Str)) {
		return false
	}
	if p.hasMin || p.hasMax {
		switch v.Type {
		case TypeNumber:
			if p.minIsTime || p.maxIsTime || (p.hasMin && v.Num < p.minNum) || (p.hasMax && v.Num > p.maxNum) {
				return false
			}
		case TypeTime:
			if p.hasMin && v.Time < timeBound(p.minIsTime, p.minMS, p.minNum) {
				return false
			}
			if p.hasMax && v.Time > timeBound(p.maxIsTime, p.maxMS, p.maxNum) {
				return false
			}
		default:
			return false
		}
	}
	if p.within != 0 && (v.Type != TypeTime || v.Time < now.Add(-p.within).UnixMilli()) {
		return false
	}
	if p.Intersects != nil && (v.Type != TypeBBox || !Intersects(v.BBox, *p.Intersects)) {
		return false
	}
	return true
}

func timeBound(isTime bool, ms int64, f float64) int64 {
	if isTime {
		return ms
	}
	return int64(f)
}

// Intersects reports whether two closed bboxes overlap; either may cross the antimeridian.
func Intersects(a, b [4]float64) bool {
	if a[1] > b[3] || b[1] > a[3] {
		return false
	}
	for _, x := range lonSpans(a) {
		for _, y := range lonSpans(b) {
			if x[0] <= y[1] && y[0] <= x[1] {
				return true
			}
		}
	}
	return false
}

func lonSpans(b [4]float64) [][2]float64 {
	if b[0] <= b[2] {
		return [][2]float64{{b[0], b[2]}}
	}
	return [][2]float64{{b[0], 180}, {-180, b[2]}}
}

// Match reports whether r matches an item at path with facets at now.
func (r *Rule) Match(path string, facets map[string]Value, now time.Time) bool {
	if r.Path != "" && path != r.Path && !strings.HasPrefix(path, r.Path+"/") {
		return false
	}
	for name, p := range r.Preds {
		v, ok := facets[name]
		if !p.Eval(v, ok, now) {
			return false
		}
	}
	return true
}

// Select returns the index of the first rule matching the item, or -1.
func Select(rules []Rule, path string, facets map[string]Value, now time.Time) int {
	for i := range rules {
		if rules[i].Match(path, facets, now) {
			return i
		}
	}
	return -1
}

// Windows lists the distinct (facet, within) pairs of rules: the moving edges at which
// items must be re-evaluated.
func Windows(rules []Rule) map[string][]time.Duration {
	out := map[string][]time.Duration{}
	for _, r := range rules {
		for name, p := range r.Preds {
			if p.within != 0 && !slices.Contains(out[name], p.within) {
				out[name] = append(out[name], p.within)
			}
		}
	}
	return out
}

// ParseDuration parses a duration: one or more <integer><unit> with units ms, s, m, h,
// d (24 h) and w (7 d), e.g. "48h", "7d", "1d12h".
func ParseDuration(s string) (time.Duration, error) {
	orig := s
	if s == "" {
		return 0, errors.New("empty duration")
	}
	var total time.Duration
	for s != "" {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 {
			return 0, fmt.Errorf("bad duration %q", orig)
		}
		n, err := strconv.ParseInt(s[:i], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bad duration %q", orig)
		}
		s = s[i:]
		j := 0
		for j < len(s) && (s[j] < '0' || s[j] > '9') {
			j++
		}
		var unit time.Duration
		switch s[:j] {
		case "ms":
			unit = time.Millisecond
		case "s":
			unit = time.Second
		case "m":
			unit = time.Minute
		case "h":
			unit = time.Hour
		case "d":
			unit = 24 * time.Hour
		case "w":
			unit = 7 * 24 * time.Hour
		default:
			return 0, fmt.Errorf("bad duration unit in %q (use ms, s, m, h, d or w)", orig)
		}
		s = s[j:]
		if n > int64(math.MaxInt64/unit) {
			return 0, fmt.Errorf("duration %q too large", orig)
		}
		total += time.Duration(n) * unit
	}
	if total <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", orig)
	}
	return total, nil
}

// ParseBytes parses a byte count: an integer, or a string with a unit B, K/KiB, M/MiB,
// G/GiB, T/TiB, P/PiB (powers of 1024) or KB, MB, GB, TB, PB (powers of 1000).
func ParseBytes(v any) (int64, error) {
	if f, ok := num(v); ok {
		if f < 0 || f != math.Trunc(f) {
			return 0, fmt.Errorf("bad byte count %v", v)
		}
		return int64(f), nil
	}
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("bad byte count %v", v)
	}
	s = strings.TrimSpace(s)
	i := len(s)
	for i > 0 && (s[i-1] < '0' || s[i-1] > '9') {
		i--
	}
	units := map[string]int64{"": 1, "B": 1, "K": 1 << 10, "KiB": 1 << 10, "M": 1 << 20, "MiB": 1 << 20, "G": 1 << 30, "GiB": 1 << 30,
		"T": 1 << 40, "TiB": 1 << 40, "P": 1 << 50, "PiB": 1 << 50, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "PB": 1e15}
	mult, ok := units[strings.TrimSpace(s[i:])]
	n, err := strconv.ParseInt(strings.TrimSpace(s[:i]), 10, 64)
	if !ok || err != nil || n < 0 || n > math.MaxInt64/mult {
		return 0, fmt.Errorf("bad byte count %q", s)
	}
	return n * mult, nil
}
