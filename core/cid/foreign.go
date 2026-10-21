package cid

import "github.com/whyrusleeping/nu/core/varint"

// IsCIDv1 reports whether b is exactly one syntactically valid CIDv1 of any codec and
// multihash. It is used for opaque foreign links inside imported atproto record contents,
// which are kept byte-exactly but never followed, fetched or provided.
func IsCIDv1(b []byte) bool {
	v, n, err := varint.Decode(b)
	if err != nil || v != 1 {
		return false
	}
	b = b[n:]
	for range 2 { // codec, multihash code
		if _, n, err = varint.Decode(b); err != nil {
			return false
		}
		b = b[n:]
	}
	l, n, err := varint.Decode(b)
	return err == nil && uint64(len(b)-n) == l
}

// ForeignString text-encodes a foreign CIDv1 in base32 ("b…"), the multibase form atproto uses.
func ForeignString(b []byte) string { return "b" + Encoding.EncodeToString(b) }

// ParseForeign parses a base32 CIDv1 string of any codec and multihash.
func ParseForeign(s string) ([]byte, error) {
	if len(s) < 2 || s[0] != 'b' {
		return nil, ErrInvalid
	}
	b, err := Encoding.DecodeString(s[1:])
	if err != nil || Encoding.EncodeToString(b) != s[1:] || !IsCIDv1(b) {
		return nil, ErrInvalid
	}
	return b, nil
}
