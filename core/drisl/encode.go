package drisl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/whyrusleeping/nu/core/cid"
)

// Typed is implemented by struct types that carry a fixed `$type`. Marshal writes the
// `$type` key and Unmarshal requires it to match.
type Typed interface{ DRISLType() string }

// Marshal encodes v as DRISL. Mapping:
//   - nil, bool, integers, float64 (no NaN/±Inf/−0), string, []byte, cid.CID, ForeignLink,
//     NegInt, slices/arrays, maps with string keys, pointers (nil → null), any;
//   - structs: exported fields keyed by the `drisl:"name[,omitempty]"` tag (default: the
//     field name; "-" skips). omitempty drops false, 0, "", nil, empty slices/maps and the
//     zero cid.CID. A Typed struct also gets its `$type` key.
//
// Output larger than the whole-object limit is refused.
func Marshal(v any) ([]byte, error) {
	e := encoder{}
	if err := e.encode(v); err != nil {
		return nil, err
	}
	if len(e.b) > cid.MaxWholeObject {
		return nil, ErrTooLarge
	}
	return e.b, nil
}

// Sum marshals v and returns its SHA-256 DRISL CID with the bytes.
func Sum(v any) (cid.CID, []byte, error) {
	b, err := Marshal(v)
	if err != nil {
		return cid.CID{}, nil, err
	}
	c, err := cid.SumCID(cid.DRISL, cid.SHA256, b)
	return c, b, err
}

type encoder struct{ b []byte }

func (e *encoder) head(major byte, arg uint64) {
	m := major << 5
	switch {
	case arg < 24:
		e.b = append(e.b, m|byte(arg))
	case arg <= 0xff:
		e.b = append(e.b, m|24, byte(arg))
	case arg <= 0xffff:
		e.b = binary.BigEndian.AppendUint16(append(e.b, m|25), uint16(arg))
	case arg <= 0xffffffff:
		e.b = binary.BigEndian.AppendUint32(append(e.b, m|26), uint32(arg))
	default:
		e.b = binary.BigEndian.AppendUint64(append(e.b, m|27), arg)
	}
}

func (e *encoder) int(i int64) {
	if i >= 0 {
		e.head(0, uint64(i))
	} else {
		e.head(1, uint64(-1-i))
	}
}

func (e *encoder) str(s string) error {
	if !utf8.ValidString(s) {
		return errors.New("drisl: string is not valid UTF-8")
	}
	e.head(3, uint64(len(s)))
	e.b = append(e.b, s...)
	return nil
}

func (e *encoder) link(b []byte) {
	e.b = append(e.b, 0xd8, 42)
	e.head(2, uint64(len(b)+1))
	e.b = append(e.b, 0)
	e.b = append(e.b, b...)
}

func (e *encoder) float(f float64) error {
	if err := checkFloat(f); err != nil {
		return err
	}
	e.b = binary.BigEndian.AppendUint64(append(e.b, 0xfb), math.Float64bits(f))
	return nil
}

func (e *encoder) encode(v any) error {
	switch x := v.(type) {
	case nil:
		e.b = append(e.b, 0xf6)
	case bool:
		if x {
			e.b = append(e.b, 0xf5)
		} else {
			e.b = append(e.b, 0xf4)
		}
	case int64:
		e.int(x)
	case int:
		e.int(int64(x))
	case uint64:
		e.head(0, x)
	case NegInt:
		if x <= math.MaxInt64 {
			return errors.New("drisl: NegInt within int64 range; use int64")
		}
		e.head(1, uint64(x))
	case float64:
		return e.float(x)
	case string:
		return e.str(x)
	case []byte:
		e.head(2, uint64(len(x)))
		e.b = append(e.b, x...)
	case cid.CID:
		if !x.Defined() {
			return errors.New("drisl: cannot encode an undefined CID")
		}
		e.link(x.Bytes())
	case ForeignLink:
		if !cid.IsCIDv1(x) {
			return errors.New("drisl: foreign link is not a CIDv1")
		}
		e.link(x)
	case []any:
		e.head(4, uint64(len(x)))
		for _, el := range x {
			if err := e.encode(el); err != nil {
				return err
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sortKeys(keys)
		e.head(5, uint64(len(keys)))
		for _, k := range keys {
			if err := e.str(k); err != nil {
				return err
			}
			if err := e.encode(x[k]); err != nil {
				return err
			}
		}
	default:
		return e.reflect(reflect.ValueOf(v))
	}
	return nil
}

func sortKeys(keys []string) {
	slices.SortFunc(keys, func(a, b string) int {
		if len(a) != len(b) {
			return len(a) - len(b)
		}
		return strings.Compare(a, b)
	})
}

var (
	cidType     = reflect.TypeFor[cid.CID]()
	foreignType = reflect.TypeFor[ForeignLink]()
	negIntType  = reflect.TypeFor[NegInt]()
	typedType   = reflect.TypeFor[Typed]()
)

func (e *encoder) reflect(rv reflect.Value) error {
	if !rv.IsValid() {
		e.b = append(e.b, 0xf6)
		return nil
	}
	switch rv.Type() {
	case cidType, foreignType, negIntType:
		return e.encode(rv.Interface())
	}
	switch rv.Kind() {
	case reflect.Bool:
		return e.encode(rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		e.int(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		e.head(0, rv.Uint())
	case reflect.Float64, reflect.Float32:
		return e.float(rv.Float())
	case reflect.String:
		return e.str(rv.String())
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			e.b = append(e.b, 0xf6)
			return nil
		}
		return e.reflect(rv.Elem())
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			e.head(2, uint64(rv.Len()))
			e.b = append(e.b, rv.Bytes()...)
			return nil
		}
		fallthrough
	case reflect.Array:
		e.head(4, uint64(rv.Len()))
		for i := range rv.Len() {
			if err := e.reflect(rv.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("drisl: map key type %s is not a string", rv.Type().Key())
		}
		keys := make([]string, 0, rv.Len())
		for _, k := range rv.MapKeys() {
			keys = append(keys, k.String())
		}
		sortKeys(keys)
		e.head(5, uint64(len(keys)))
		for _, k := range keys {
			if err := e.str(k); err != nil {
				return err
			}
			if err := e.reflect(rv.MapIndex(reflect.ValueOf(k).Convert(rv.Type().Key()))); err != nil {
				return err
			}
		}
	case reflect.Struct:
		return e.structure(rv)
	default:
		return fmt.Errorf("drisl: cannot encode %s", rv.Type())
	}
	return nil
}

func (e *encoder) structure(rv reflect.Value) error {
	info := fieldsOf(rv.Type())
	type kv struct {
		k string
		v reflect.Value
	}
	out := make([]kv, 0, len(info.fields)+1)
	for _, f := range info.fields {
		fv := rv.FieldByIndex(f.index)
		if f.omitempty && isEmpty(fv) {
			continue
		}
		out = append(out, kv{f.name, fv})
	}
	if info.typ != "" {
		out = append(out, kv{"$type", reflect.ValueOf(info.typ)})
	}
	slices.SortFunc(out, func(a, b kv) int {
		if len(a.k) != len(b.k) {
			return len(a.k) - len(b.k)
		}
		return strings.Compare(a.k, b.k)
	})
	e.head(5, uint64(len(out)))
	for _, p := range out {
		if err := e.str(p.k); err != nil {
			return err
		}
		if err := e.reflect(p.v); err != nil {
			return err
		}
	}
	return nil
}

func isEmpty(v reflect.Value) bool {
	if v.Type() == cidType {
		return !v.Interface().(cid.CID).Defined()
	}
	switch v.Kind() {
	case reflect.Slice, reflect.Map, reflect.String:
		return v.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	}
	return v.IsZero()
}

type field struct {
	name      string
	index     []int
	omitempty bool
}

type structInfo struct {
	fields []field
	byName map[string]*field
	typ    string // fixed $type, for Typed structs
}

var structCache sync.Map // reflect.Type -> *structInfo

func fieldsOf(t reflect.Type) *structInfo {
	if v, ok := structCache.Load(t); ok {
		return v.(*structInfo)
	}
	info := &structInfo{byName: map[string]*field{}}
	for _, sf := range reflect.VisibleFields(t) {
		if !sf.IsExported() || sf.Anonymous {
			continue
		}
		name, opts, _ := strings.Cut(sf.Tag.Get("drisl"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		info.fields = append(info.fields, field{name: name, index: sf.Index, omitempty: opts == "omitempty"})
	}
	for i := range info.fields {
		info.byName[info.fields[i].name] = &info.fields[i]
	}
	if t.Implements(typedType) {
		info.typ = reflect.Zero(t).Interface().(Typed).DRISLType()
	}
	structCache.Store(t, info)
	return info
}
