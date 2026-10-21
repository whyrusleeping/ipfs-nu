// Package wire implements the bounded wire formats of Nu's libp2p subset.
package wire

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/whyrusleeping/nu/core/varint"
)

var (
	ErrLimit     = errors.New("wire: size limit exceeded")
	ErrFormat    = errors.New("wire: invalid encoding")
	ErrSignature = errors.New("wire: invalid signature")
)

// Field is one protobuf field. Bytes aliases the input. Groups are not supported.
type Field struct {
	Number int
	Type   int
	Uint   uint64
	Bytes  []byte
}

// ProtoReader is a non-recursive protobuf reader. Callers bound nested messages
// explicitly; it never allocates from lengths supplied by the remote peer.
type ProtoReader struct{ rest []byte }

func NewProtoReader(b []byte, maxBytes int) (*ProtoReader, error) {
	if maxBytes < 0 || len(b) > maxBytes {
		return nil, ErrLimit
	}
	return &ProtoReader{rest: b}, nil
}

func (r *ProtoReader) Next() (Field, error) {
	var f Field
	if len(r.rest) == 0 {
		return f, io.EOF
	}
	tag, n, err := varint.Decode(r.rest)
	if err != nil {
		return f, err
	}
	r.rest = r.rest[n:]
	if tag>>3 == 0 || tag>>3 > 1<<29-1 {
		return f, ErrFormat
	}
	f.Number = int(tag >> 3)
	f.Type = int(tag & 7)
	switch f.Type {
	case 0:
		// Protobuf uint64 values (unlike multiformats lengths/tags) may use
		// all 64 bits, requiring ten bytes. Still reject nonminimal encodings.
		v, n := binary.Uvarint(r.rest)
		if n <= 0 {
			return f, ErrFormat
		}
		if n != len(binary.AppendUvarint(nil, v)) {
			return f, ErrFormat
		}
		f.Uint = v
		r.rest = r.rest[n:]
	case 1, 5:
		n := 8
		if f.Type == 5 {
			n = 4
		}
		if len(r.rest) < n {
			return f, io.ErrUnexpectedEOF
		}
		f.Bytes = r.rest[:n]
		r.rest = r.rest[n:]
	case 2:
		v, n, err := varint.Decode(r.rest)
		if err != nil {
			return f, err
		}
		r.rest = r.rest[n:]
		if v > uint64(len(r.rest)) {
			return f, io.ErrUnexpectedEOF
		}
		f.Bytes = r.rest[:int(v)]
		r.rest = r.rest[int(v):]
	default:
		return f, ErrFormat
	}
	return f, nil
}

// AppendBytes and AppendUint append protobuf fields in caller-specified order.
func AppendBytes(b []byte, number int, value []byte) []byte {
	b = varint.Append(b, uint64(number)<<3|2)
	b = varint.Append(b, uint64(len(value)))
	return append(b, value...)
}
func AppendUint(b []byte, number int, value uint64) []byte {
	b = varint.Append(b, uint64(number)<<3)
	return binary.AppendUvarint(b, value)
}
