package blob

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"lukechampine.com/blake3"
	"lukechampine.com/blake3/bao"

	"github.com/whyrusleeping/nu/core/cid"
)

func baoInput(n int) []byte {
	var in []byte
	for i := uint32(1); len(in) < n; i++ {
		in = binary.LittleEndian.AppendUint32(in, i)
	}
	return in[:n]
}

type vectorFile struct {
	Hash []struct {
		InputLen int    `json:"input_len"`
		BaoHash  string `json:"bao_hash"`
	} `json:"hash"`
	Outboard []struct {
		InputLen     int    `json:"input_len"`
		OutputLen    int    `json:"output_len"`
		EncodedBlake string `json:"encoded_blake3"`
	} `json:"outboard"`
	Slice []struct {
		InputLen int    `json:"input_len"`
		BaoHash  string `json:"bao_hash"`
		Slices   []struct {
			Start       uint64 `json:"start"`
			Len         uint64 `json:"len"`
			OutputLen   int    `json:"output_len"`
			OutputBlake string `json:"output_blake3"`
			Corruptions []int  `json:"corruptions"`
		} `json:"slices"`
	} `json:"slice"`
}

// writerAt is a growable io.WriterAt.
type writerAt struct{ b []byte }

func (w *writerAt) WriteAt(p []byte, off int64) (int, error) {
	if end := int(off) + len(p); end > len(w.b) {
		w.b = append(w.b, make([]byte, end-len(w.b))...)
	}
	return copy(w.b[off:], p), nil
}

// checkAt compares written data with the expected blob at each offset.
type checkAt struct {
	in  []byte
	bad bool
}

func (c *checkAt) WriteAt(p []byte, off int64) (int, error) {
	if !bytes.Equal(p, c.in[off:off+int64(len(p))]) {
		c.bad = true
	}
	return len(p), nil
}

func TestBaoVectors(t *testing.T) {
	for file, glog := range map[string]uint{"bao_test_vectors.json": 0, "abao_test_vectors.json": GroupLog} {
		raw, err := os.ReadFile(filepath.Join("../../testdata/vectors/bao", file))
		if err != nil {
			t.Fatal(err)
		}
		var v vectorFile
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		for _, c := range v.Hash {
			mh, _, _ := Sum(bytes.NewReader(baoInput(c.InputLen)))
			if hex.EncodeToString(mh.Digest[:]) != c.BaoHash {
				t.Errorf("%s hash %d", file, c.InputLen)
			}
		}
		for _, c := range v.Outboard {
			in := baoInput(c.InputLen)
			var ob []byte
			if glog == GroupLog {
				w := &writerAt{}
				if _, err := Outboard(w, bytes.NewReader(in), uint64(len(in))); err != nil {
					t.Fatal(err)
				}
				ob = w.b
				if uint64(len(ob)) != OutboardSize(uint64(len(in))) {
					t.Errorf("OutboardSize(%d)", len(in))
				}
			} else {
				ob, _ = bao.EncodeBuf(in, int(glog), true)
			}
			if sum := blake3.Sum256(ob); len(ob) != c.OutputLen || hex.EncodeToString(sum[:]) != c.EncodedBlake {
				t.Errorf("%s outboard %d", file, c.InputLen)
			}
		}
		for _, c := range v.Slice {
			in := baoInput(c.InputLen)
			ob, _ := bao.EncodeBuf(in, int(glog), true)
			root := blake3.Sum256(in)
			size := uint64(len(in))
			for _, s := range c.Slices {
				var out bytes.Buffer
				if err := extract(&out, bytes.NewReader(in), bytes.NewReader(ob), size, s.Start, s.Len, glog); err != nil {
					t.Fatalf("%s extract %d/%d+%d: %v", file, c.InputLen, s.Start, s.Len, err)
				}
				slice := out.Bytes()
				if sum := blake3.Sum256(slice); len(slice) != s.OutputLen || hex.EncodeToString(sum[:]) != s.OutputBlake {
					t.Errorf("%s slice %d/%d+%d: %d bytes", file, c.InputLen, s.Start, s.Len, len(slice))
					continue
				}
				chk := &checkAt{in: in}
				if err := decode(bytes.NewReader(slice), root, size, s.Start, s.Len, glog, Sink{Data: chk}); err != nil || chk.bad {
					t.Errorf("%s decode %d/%d+%d: %v", file, c.InputLen, s.Start, s.Len, err)
				}
				for _, i := range s.Corruptions {
					bad := bytes.Clone(slice)
					bad[i] ^= 1
					if decode(bytes.NewReader(bad), root, size, s.Start, s.Len, glog, Sink{}) == nil {
						t.Errorf("%s corruption %d/%d+%d@%d accepted", file, c.InputLen, s.Start, s.Len, i)
					}
				}
			}
		}
	}
}

func TestRangeSliceAndCorruption(t *testing.T) {
	size := uint64(10*GroupSize + 123)
	in := make([]byte, size)
	blake3.New(32, nil).XOF().Read(in)
	ob := &writerAt{}
	mh, err := Outboard(ob, bytes.NewReader(in), size)
	if err != nil || mh != cid.Sum(cid.BLAKE3, in) {
		t.Fatal("outboard root differs from BLAKE3")
	}
	// Our slices match the library's for group-aligned ranges, and a receiver that collects
	// slices covering every group rebuilds the full outboard.
	r := Range{3, 7}
	var mine bytes.Buffer
	if err := ExtractSlice(&mine, bytes.NewReader(in), bytes.NewReader(ob.b), size, r); err != nil {
		t.Fatal(err)
	}
	off, n := r.Bytes(size)
	var lib bytes.Buffer
	if err := bao.ExtractSlice(&lib, bytes.NewReader(in[off:off+n]), bytes.NewReader(ob.b), GroupLog, off, n); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mine.Bytes(), lib.Bytes()) {
		t.Fatal("slice differs from lukechampine.com/blake3/bao")
	}
	data, rob := &writerAt{}, &writerAt{}
	for _, rr := range []Range{{0, 3}, {3, 7}, {7, Groups(size)}} {
		var s bytes.Buffer
		_ = ExtractSlice(&s, bytes.NewReader(in), bytes.NewReader(ob.b), size, rr)
		if err := DecodeSlice(&s, mh, size, rr, Sink{Data: data, Outboard: rob}); err != nil || s.Len() != 0 {
			t.Fatalf("decode %v: %v", rr, err)
		}
	}
	if !bytes.Equal(data.b, in) || !bytes.Equal(rob.b, ob.b) {
		t.Fatal("reassembled data/outboard differ")
	}

	// A flipped byte in group 5 is detected in group 5, after groups 0-4 were delivered.
	var full bytes.Buffer
	all := Range{0, Groups(size)}
	_ = ExtractSlice(&full, bytes.NewReader(in), bytes.NewReader(ob.b), size, all)
	bad := full.Bytes()
	i := bytes.Index(bad, in[5*GroupSize+100:5*GroupSize+132])
	bad[i] ^= 0x40
	var got []uint64
	err = DecodeSlice(bytes.NewReader(bad), mh, size, all, Sink{Group: func(g uint64) { got = append(got, g) }})
	var ce *CorruptError
	if !errors.As(err, &ce) || ce.Group != 5 || len(got) != 5 {
		t.Fatalf("err %v, delivered %v", err, got)
	}
}

func TestByteRange(t *testing.T) {
	size := uint64(3*GroupSize + 10)
	for _, c := range []struct {
		s, e uint64
		want Range
	}{
		{0, 1, Range{0, 1}},
		{GroupSize - 1, GroupSize + 1, Range{0, 2}},
		{GroupSize, 2 * GroupSize, Range{1, 2}},
		{3 * GroupSize, 1 << 40, Range{3, 4}},
		{1 << 40, 1 << 41, Range{3, 4}},
	} {
		if got := ByteRange(c.s, c.e, size); got != c.want {
			t.Errorf("ByteRange(%d,%d) = %v, want %v", c.s, c.e, got, c.want)
		}
	}
	if HasOutboard(GroupSize) || !HasOutboard(GroupSize+1) {
		t.Fatal("outboard threshold")
	}
	small := []byte("hello")
	if VerifyWhole(small, cid.Sum(cid.BLAKE3, small)) != nil || VerifyWhole([]byte("hellp"), cid.Sum(cid.BLAKE3, small)) == nil {
		t.Fatal("whole verification")
	}
}

func FuzzDecodeSlice(f *testing.F) {
	in := baoInput(3*GroupSize + 7)
	ob, root := bao.EncodeBuf(in, GroupLog, true)
	var s bytes.Buffer
	_ = ExtractSlice(&s, bytes.NewReader(in), bytes.NewReader(ob), uint64(len(in)), Range{1, 3})
	f.Add(s.Bytes())
	mh := cid.Multihash{Hash: cid.BLAKE3, Digest: root}
	f.Fuzz(func(t *testing.T, b []byte) {
		data := &writerAt{}
		if DecodeSlice(bytes.NewReader(b), mh, uint64(len(in)), Range{1, 3}, Sink{Data: data}) == nil {
			if !bytes.Equal(data.b[GroupSize:], in[GroupSize:3*GroupSize]) {
				t.Fatal("accepted wrong data")
			}
		}
	})
}
