package drisl

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/whyrusleeping/nu/core/cid"
)

// JSON form (API, CLI, gateway) uses the atproto conventions: links are {"$link": "<cid>"}
// and bytes are {"$bytes": "<base64>"} (standard alphabet, unpadded on output).

// ToJSON renders a generic value (or anything Marshal accepts) as JSON.
func ToJSON(v any) ([]byte, error) {
	if !isGeneric(v) {
		b, err := Marshal(v)
		if err != nil {
			return nil, err
		}
		if v, err = Decode(b); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	if err := writeJSON(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func isGeneric(v any) bool {
	switch v.(type) {
	case nil, bool, int64, uint64, NegInt, float64, string, []byte, []any, map[string]any, cid.CID, ForeignLink:
		return true
	}
	return false
}

func writeJSON(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(x))
	case int64:
		buf.WriteString(strconv.FormatInt(x, 10))
	case int:
		buf.WriteString(strconv.Itoa(x))
	case uint64:
		buf.WriteString(strconv.FormatUint(x, 10))
	case NegInt:
		// -1 - x, with x > MaxInt64.
		buf.WriteString("-" + negString(uint64(x)))
	case float64:
		if err := checkFloat(x); err != nil {
			return err
		}
		s := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0" // keep floats distinguishable from integers
		}
		buf.WriteString(s)
	case string:
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(b)
	case []byte:
		buf.WriteString(`{"$bytes":"` + base64.RawStdEncoding.EncodeToString(x) + `"}`)
	case cid.CID:
		buf.WriteString(`{"$link":"` + x.String() + `"}`)
	case ForeignLink:
		buf.WriteString(`{"$link":"` + x.String() + `"}`)
	case []any:
		buf.WriteByte('[')
		for i, el := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSON(buf, el); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sortKeys(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeJSON(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("drisl: cannot render %T as JSON", v)
	}
	return nil
}

// negString returns the decimal digits of 1 + x for x up to 2^64-1.
func negString(x uint64) string {
	if x < math.MaxUint64 {
		return strconv.FormatUint(x+1, 10)
	}
	return "18446744073709551616"
}

// JSONOptions controls FromJSON.
type JSONOptions struct {
	MaxSize int // 0 means cid.MaxWholeObject
	// Atproto applies the atproto data model: integral numbers written with a fraction or
	// exponent (123.0) are integers and other floats are rejected.
	Atproto bool
	// ForeignLinks accepts non-DASL CIDv1 strings in $link (atproto record contents).
	ForeignLinks bool
}

// FromJSON parses atproto-convention JSON into a generic value.
func FromJSON(data []byte, o JSONOptions) (any, error) {
	max := o.MaxSize
	if max == 0 {
		max = cid.MaxWholeObject
	}
	if len(data) > max {
		return nil, ErrTooLarge
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("drisl: invalid JSON: %w", err)
	}
	if dec.More() {
		return nil, errors.New("drisl: trailing data after JSON value")
	}
	return fromJSON(raw, o, 0)
}

func fromJSON(v any, o JSONOptions, depth int) (any, error) {
	if depth > DefaultMaxDepth {
		return nil, ErrTooDeep
	}
	switch x := v.(type) {
	case nil, bool, string:
		return x, nil
	case json.Number:
		return jsonNumber(string(x), o)
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			var err error
			if out[i], err = fromJSON(el, o, depth+1); err != nil {
				return nil, err
			}
		}
		return out, nil
	case map[string]any:
		if l, ok := x["$link"]; ok {
			s, ok := l.(string)
			if !ok || len(x) != 1 {
				return nil, errors.New(`drisl: "$link" must be the only key and a string`)
			}
			if c, err := cid.Parse(s); err == nil {
				return c, nil
			}
			if o.ForeignLinks {
				if b, err := cid.ParseForeign(s); err == nil {
					return ForeignLink(b), nil
				}
			}
			return nil, fmt.Errorf("drisl: invalid $link %q", s)
		}
		if l, ok := x["$bytes"]; ok {
			s, ok := l.(string)
			if !ok || len(x) != 1 {
				return nil, errors.New(`drisl: "$bytes" must be the only key and a string`)
			}
			b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
			if err != nil {
				return nil, fmt.Errorf("drisl: invalid $bytes: %w", err)
			}
			return b, nil
		}
		out := make(map[string]any, len(x))
		for k, el := range x {
			var err error
			if out[k], err = fromJSON(el, o, depth+1); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("drisl: unexpected JSON value %T", v)
}

func jsonNumber(s string, o JSONOptions) (any, error) {
	if !strings.ContainsAny(s, ".eE") {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, nil
		}
		if !o.Atproto {
			if u, err := strconv.ParseUint(s, 10, 64); err == nil {
				return u, nil
			}
			if strings.HasPrefix(s, "-") {
				// -1 - n for n up to 2^64-1, i.e. values down to -2^64.
				if u, err := strconv.ParseUint(s[1:], 10, 64); err == nil && u > 0 {
					return NegInt(u - 1), nil
				}
				if s[1:] == "18446744073709551616" {
					return NegInt(math.MaxUint64), nil
				}
			}
		}
		return nil, fmt.Errorf("drisl: integer %s out of range", s)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("drisl: invalid number %s", s)
	}
	if o.Atproto {
		if f != math.Trunc(f) || math.Abs(f) > 1<<53 {
			return nil, errors.New("drisl: floats are not allowed in the atproto data model")
		}
		return int64(f), nil
	}
	if f == 0 {
		f = 0 // normalise -0
	}
	return f, checkFloat(f)
}

// ValidateAtproto checks a generic value against the atproto data model as a record: the
// top level is a map, no floats, integers fit int64, every `$type` is a non-empty string and
// `{$type: "blob"}` refs carry ref (link), mimeType (string) and size (integer). Legacy blob
// refs ({cid, mimeType}) are plain maps and pass unchanged.
func ValidateAtproto(v any) error {
	if _, ok := v.(map[string]any); !ok {
		return errors.New("atproto: record must be a map")
	}
	return validateAtproto(v, 0)
}

func validateAtproto(v any, depth int) error {
	if depth > DefaultMaxDepth {
		return ErrTooDeep
	}
	switch x := v.(type) {
	case float64:
		return errors.New("atproto: floats are not allowed")
	case uint64, NegInt:
		return errors.New("atproto: integer outside int64")
	case []any:
		for _, el := range x {
			if err := validateAtproto(el, depth+1); err != nil {
				return err
			}
		}
	case map[string]any:
		if t, ok := x["$type"]; ok {
			s, ok := t.(string)
			if !ok || s == "" {
				return errors.New("atproto: $type must be a non-empty string")
			}
			if s == "blob" {
				if _, ok := x["ref"].(cid.CID); !ok {
					return errors.New("atproto: blob ref must be a link")
				}
				if _, ok := x["mimeType"].(string); !ok {
					return errors.New("atproto: blob mimeType must be a string")
				}
				if _, ok := x["size"].(int64); !ok {
					return errors.New("atproto: blob size must be an integer")
				}
			}
		}
		for _, el := range x {
			if err := validateAtproto(el, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// Links calls fn for every DASL/BDASL link in a generic value, in encoded (canonical) order.
// Foreign links are not reported.
func Links(v any, fn func(cid.CID)) {
	switch x := v.(type) {
	case cid.CID:
		fn(x)
	case []any:
		for _, el := range x {
			Links(el, fn)
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sortKeys(keys)
		for _, k := range keys {
			Links(x[k], fn)
		}
	}
}
