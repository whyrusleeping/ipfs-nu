package drisl

import (
	"fmt"
	"reflect"

	"github.com/whyrusleeping/nu/core/cid"
)

// Assign stores the generic value val (as produced by Decode) into the value pointed to by
// ptr, using the mapping described at Marshal. Unknown map keys are ignored; missing keys
// leave fields at their zero value.
func Assign(val any, ptr any) error {
	rv := reflect.ValueOf(ptr)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("drisl: Assign needs a non-nil pointer, got %T", ptr)
	}
	return assign(val, rv.Elem(), "")
}

type typeError struct {
	path string
	want reflect.Type
	got  any
}

func (e *typeError) Error() string {
	p := e.path
	if p == "" {
		p = "value"
	}
	return fmt.Sprintf("drisl: %s: cannot store %T in %s", p, e.got, e.want)
}

func assign(val any, rv reflect.Value, path string) error {
	t := rv.Type()
	bad := func() error { return &typeError{path, t, val} }
	switch t {
	case cidType:
		c, ok := val.(cid.CID)
		if !ok {
			return bad()
		}
		rv.Set(reflect.ValueOf(c))
		return nil
	case negIntType:
		n, ok := val.(NegInt)
		if !ok {
			return bad()
		}
		rv.SetUint(uint64(n))
		return nil
	case foreignType:
		switch x := val.(type) {
		case ForeignLink:
			rv.Set(reflect.ValueOf(x))
		case cid.CID:
			rv.Set(reflect.ValueOf(ForeignLink(x.Bytes())))
		default:
			return bad()
		}
		return nil
	}
	if t.Kind() == reflect.Interface {
		if val == nil {
			rv.SetZero()
			return nil
		}
		v := reflect.ValueOf(val)
		if !v.Type().AssignableTo(t) {
			return bad()
		}
		rv.Set(v)
		return nil
	}
	if t.Kind() == reflect.Pointer {
		if val == nil {
			rv.SetZero()
			return nil
		}
		p := reflect.New(t.Elem())
		if err := assign(val, p.Elem(), path); err != nil {
			return err
		}
		rv.Set(p)
		return nil
	}
	switch t.Kind() {
	case reflect.Bool:
		b, ok := val.(bool)
		if !ok {
			return bad()
		}
		rv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i, ok := val.(int64)
		if !ok || rv.OverflowInt(i) {
			return bad()
		}
		rv.SetInt(i)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		var u uint64
		switch x := val.(type) {
		case int64:
			if x < 0 {
				return bad()
			}
			u = uint64(x)
		case uint64:
			u = x
		default:
			return bad()
		}
		if rv.OverflowUint(u) {
			return bad()
		}
		rv.SetUint(u)
	case reflect.Float64, reflect.Float32:
		f, ok := val.(float64)
		if !ok {
			return bad()
		}
		rv.SetFloat(f)
	case reflect.String:
		s, ok := val.(string)
		if !ok {
			return bad()
		}
		rv.SetString(s)
	case reflect.Slice:
		if val == nil {
			rv.SetZero()
			return nil
		}
		if t.Elem().Kind() == reflect.Uint8 {
			b, ok := val.([]byte)
			if !ok {
				return bad()
			}
			rv.SetBytes(b)
			return nil
		}
		a, ok := val.([]any)
		if !ok {
			return bad()
		}
		s := reflect.MakeSlice(t, len(a), len(a))
		for i, el := range a {
			if err := assign(el, s.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		rv.Set(s)
	case reflect.Array:
		a, ok := val.([]any)
		if !ok || len(a) != t.Len() {
			return bad()
		}
		for i, el := range a {
			if err := assign(el, rv.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		if val == nil {
			rv.SetZero()
			return nil
		}
		m, ok := val.(map[string]any)
		if !ok || t.Key().Kind() != reflect.String {
			return bad()
		}
		out := reflect.MakeMapWithSize(t, len(m))
		for k, el := range m {
			ev := reflect.New(t.Elem()).Elem()
			if err := assign(el, ev, path+"."+k); err != nil {
				return err
			}
			out.SetMapIndex(reflect.ValueOf(k).Convert(t.Key()), ev)
		}
		rv.Set(out)
	case reflect.Struct:
		m, ok := val.(map[string]any)
		if !ok {
			return bad()
		}
		info := fieldsOf(t)
		if info.typ != "" {
			if got, _ := m["$type"].(string); got != info.typ {
				return fmt.Errorf("drisl: %s: $type %q, want %q", path, m["$type"], info.typ)
			}
		}
		rv.SetZero()
		for k, el := range m {
			f := info.byName[k]
			if f == nil {
				continue
			}
			if err := assign(el, rv.FieldByIndex(f.index), path+"."+k); err != nil {
				return err
			}
		}
	default:
		return bad()
	}
	return nil
}
