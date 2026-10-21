package varint

import (
	"bytes"
	"testing"
)

func TestVectors(t *testing.T) {
	// From the multiformats unsigned-varint spec.
	for _, c := range []struct {
		v   uint64
		enc []byte
	}{
		{1, []byte{0x01}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{255, []byte{0xff, 0x01}},
		{300, []byte{0xac, 0x02}},
		{16384, []byte{0x80, 0x80, 0x01}},
		{1<<63 - 1, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}},
	} {
		if got := Append(nil, c.v); !bytes.Equal(got, c.enc) || Len(c.v) != len(c.enc) {
			t.Errorf("encode %d = %x, want %x", c.v, got, c.enc)
		}
		v, n, err := Decode(c.enc)
		if err != nil || v != c.v || n != len(c.enc) {
			t.Errorf("decode %x = %d,%d,%v", c.enc, v, n, err)
		}
	}
	for _, bad := range [][]byte{{}, {0x80}, {0x80, 0x00}, {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}} {
		if _, _, err := Decode(bad); err == nil {
			t.Errorf("decode %x: want error", bad)
		}
	}
}

// FuzzDecode is the project's fuzz-target template: seeds live in f.Add and
// testdata/fuzz/FuzzDecode, and run as ordinary tests in `go test`.
func FuzzDecode(f *testing.F) {
	f.Add([]byte{0x01})
	f.Add([]byte{0xac, 0x02})
	f.Add([]byte{0x80, 0x00})
	f.Fuzz(func(t *testing.T, b []byte) {
		v, n, err := Decode(b)
		if err != nil {
			return
		}
		if !bytes.Equal(Append(nil, v), b[:n]) {
			t.Fatalf("decode %x = %d does not re-encode", b[:n], v)
		}
		r, err := Read(bytes.NewReader(b))
		if err != nil || r != v {
			t.Fatalf("Read = %d,%v; Decode = %d", r, err, v)
		}
	})
}
