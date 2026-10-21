// Package drisl is a strict DRISL (deterministic DAG-CBOR, CBOR/c tag-42 profile) codec.
//
// Generic values decode to: nil, bool, int64, uint64 (only above MaxInt64), NegInt (only
// below MinInt64), float64, string, []byte, []any, map[string]any, cid.CID and, when
// allowed, ForeignLink. Marshal accepts those plus Go structs, slices, maps with string keys,
// pointers and the other integer kinds (see Marshal).
package drisl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/whyrusleeping/nu/core/cid"
)

// NegInt is a CBOR negative integer below math.MinInt64; its value is -1 - N.
type NegInt uint64

// ForeignLink is a syntactically valid CIDv1 that is not a DASL/BDASL CID (binary form,
// without the 0x00 tag-42 prefix). It appears only when DecOptions.ForeignLinks is set, for
// atproto record contents, and is kept byte-exactly but never followed.
type ForeignLink []byte

func (f ForeignLink) String() string { return cid.ForeignString(f) }

const (
	// MaxNodeSize is the cap on DRISL nodes Nu produces (D1).
	MaxNodeSize = 1 << 20
	// DefaultMaxDepth bounds nesting of untrusted input.
	DefaultMaxDepth = 128
)

var (
	ErrNonCanonical = errors.New("drisl: non-canonical encoding")
	ErrTooLarge     = fmt.Errorf("drisl: object exceeds %d bytes", cid.MaxWholeObject)
	ErrTooDeep      = errors.New("drisl: nesting too deep")
	ErrTruncated    = errors.New("drisl: truncated input")
)

// DecOptions controls decoding of untrusted input. The zero value is strict generic DRISL
// with the default limits.
type DecOptions struct {
	MaxSize  int // input size limit; 0 means cid.MaxWholeObject
	MaxDepth int // 0 means DefaultMaxDepth
	// Atproto applies the atproto data model at the codec level: no floats, integers must
	// fit in int64.
	Atproto bool
	// ForeignLinks keeps non-DASL CIDv1 links as ForeignLink instead of failing. Use it only
	// for atproto record contents.
	ForeignLinks bool
}

// AtprotoRecord is the decoding profile for imported atproto record contents.
var AtprotoRecord = DecOptions{Atproto: true, ForeignLinks: true}

// Unmarshal decodes strict DRISL into v (a pointer; see Marshal for the mapping). It uses the
// zero DecOptions.
func Unmarshal(data []byte, v any) error { return DecOptions{}.Unmarshal(data, v) }

// Unmarshal decodes data into v under these options.
func (o DecOptions) Unmarshal(data []byte, v any) error {
	val, err := o.Decode(data)
	if err != nil {
		return err
	}
	return Assign(val, v)
}

// Decode decodes data into a generic value.
func Decode(data []byte) (any, error) { return DecOptions{}.Decode(data) }

// Decode decodes data into a generic value under these options.
func (o DecOptions) Decode(data []byte) (any, error) {
	max := o.MaxSize
	if max == 0 {
		max = cid.MaxWholeObject
	}
	if len(data) > max {
		return nil, ErrTooLarge
	}
	d := decoder{b: data, opts: o, maxDepth: o.MaxDepth}
	if d.maxDepth == 0 {
		d.maxDepth = DefaultMaxDepth
	}
	v, err := d.value(0)
	if err != nil {
		return nil, err
	}
	if d.p != len(d.b) {
		return nil, errors.New("drisl: trailing bytes after object")
	}
	return v, nil
}

type decoder struct {
	b        []byte
	p        int
	opts     DecOptions
	maxDepth int
}

func (d *decoder) remaining() int { return len(d.b) - d.p }

// head reads a major type and argument, enforcing minimal encoding and definite lengths.
func (d *decoder) head() (major byte, info byte, arg uint64, err error) {
	if d.p >= len(d.b) {
		return 0, 0, 0, ErrTruncated
	}
	c := d.b[d.p]
	d.p++
	major, info = c>>5, c&31
	if info < 24 {
		return major, info, uint64(info), nil
	}
	var n int
	switch info {
	case 24:
		n = 1
	case 25:
		n = 2
	case 26:
		n = 4
	case 27:
		n = 8
	case 31:
		return 0, 0, 0, errors.New("drisl: indefinite lengths are not allowed")
	default:
		return 0, 0, 0, fmt.Errorf("drisl: reserved additional info %d", info)
	}
	if d.remaining() < n {
		return 0, 0, 0, ErrTruncated
	}
	s := d.b[d.p : d.p+n]
	d.p += n
	switch n {
	case 1:
		arg = uint64(s[0])
	case 2:
		arg = uint64(binary.BigEndian.Uint16(s))
	case 4:
		arg = uint64(binary.BigEndian.Uint32(s))
	case 8:
		arg = binary.BigEndian.Uint64(s)
	}
	if major == 7 {
		return major, info, arg, nil // floats/simple values: checked by the caller
	}
	if (n == 1 && arg < 24) || (n == 2 && arg <= 0xff) || (n == 4 && arg <= 0xffff) || (n == 8 && arg <= 0xffffffff) {
		return 0, 0, 0, ErrNonCanonical
	}
	return major, info, arg, nil
}

func (d *decoder) bytesArg(arg uint64) ([]byte, error) {
	if arg > uint64(d.remaining()) {
		return nil, ErrTruncated
	}
	s := d.b[d.p : d.p+int(arg)]
	d.p += int(arg)
	return s, nil
}

func (d *decoder) value(depth int) (any, error) {
	if depth > d.maxDepth {
		return nil, ErrTooDeep
	}
	major, info, arg, err := d.head()
	if err != nil {
		return nil, err
	}
	switch major {
	case 0:
		if arg > math.MaxInt64 {
			if d.opts.Atproto {
				return nil, errors.New("drisl: integer outside the atproto int64 range")
			}
			return arg, nil
		}
		return int64(arg), nil
	case 1:
		if arg > math.MaxInt64 {
			if d.opts.Atproto {
				return nil, errors.New("drisl: integer outside the atproto int64 range")
			}
			return NegInt(arg), nil
		}
		return -1 - int64(arg), nil
	case 2:
		s, err := d.bytesArg(arg)
		if err != nil {
			return nil, err
		}
		return append([]byte{}, s...), nil
	case 3:
		s, err := d.bytesArg(arg)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(s) {
			return nil, errors.New("drisl: invalid UTF-8 in string")
		}
		return string(s), nil
	case 4:
		// Every element takes at least one byte, so remaining input bounds allocation.
		if arg > uint64(d.remaining()) {
			return nil, ErrTruncated
		}
		a := make([]any, 0, arg)
		for range arg {
			v, err := d.value(depth + 1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		return a, nil
	case 5:
		if arg > uint64(d.remaining())/2 {
			return nil, ErrTruncated
		}
		m := make(map[string]any, arg)
		var prev string
		for i := range arg {
			km, _, kl, err := d.head()
			if err != nil {
				return nil, err
			}
			if km != 3 {
				return nil, errors.New("drisl: map keys must be strings")
			}
			kb, err := d.bytesArg(kl)
			if err != nil {
				return nil, err
			}
			if !utf8.Valid(kb) {
				return nil, errors.New("drisl: invalid UTF-8 in map key")
			}
			k := string(kb)
			if i > 0 && !keyLess(prev, k) {
				if prev == k {
					return nil, errors.New("drisl: duplicate map key")
				}
				return nil, errors.New("drisl: map keys out of canonical order")
			}
			prev = k
			v, err := d.value(depth + 1)
			if err != nil {
				return nil, err
			}
			m[k] = v
		}
		return m, nil
	case 6:
		if arg != 42 {
			return nil, fmt.Errorf("drisl: tag %d not allowed (only 42)", arg)
		}
		bm, _, bl, err := d.head()
		if err != nil {
			return nil, err
		}
		if bm != 2 {
			return nil, errors.New("drisl: tag 42 must wrap a byte string")
		}
		s, err := d.bytesArg(bl)
		if err != nil {
			return nil, err
		}
		if len(s) == 0 || s[0] != 0 {
			return nil, errors.New("drisl: tag-42 CID lacks the 0x00 prefix")
		}
		c, err := cid.Decode(s[1:])
		if err == nil {
			return c, nil
		}
		if d.opts.ForeignLinks && cid.IsCIDv1(s[1:]) {
			return ForeignLink(append([]byte{}, s[1:]...)), nil
		}
		return nil, errors.New("drisl: tag-42 value is not a DASL/BDASL CID")
	case 7:
		switch info {
		case 20:
			return false, nil
		case 21:
			return true, nil
		case 22:
			return nil, nil
		case 27:
			if d.opts.Atproto {
				return nil, errors.New("drisl: floats are not allowed in the atproto data model")
			}
			f := math.Float64frombits(arg)
			if err := checkFloat(f); err != nil {
				return nil, err
			}
			return f, nil
		case 25, 26:
			return nil, errors.New("drisl: only 64-bit floats are allowed")
		}
		return nil, errors.New("drisl: simple value not allowed (only false, true, null)")
	}
	panic("unreachable")
}

func checkFloat(f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return errors.New("drisl: NaN and infinities are not allowed")
	}
	if f == 0 && math.Signbit(f) {
		return errors.New("drisl: negative zero is not allowed")
	}
	return nil
}

// keyLess is DRISL map key order: shorter keys first, then bytewise.
func keyLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
