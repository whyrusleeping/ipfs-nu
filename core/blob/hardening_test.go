package blob

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/whyrusleeping/nu/nutest/slow"
	"lukechampine.com/blake3"
)

// Regenerate with experiments/bao-vectors (independent Rust bao-tree 0.16.1).
func TestBaoTreeVectors(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/vectors/bao/bao-tree-16k.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Size         uint64 `json:"size"`
		Root         string `json:"root"`
		OutboardHash string `json:"outboard_hash"`
		Slices       []struct {
			Start, End uint64
			Hash       string
			Bytes      int
		}
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 10 {
		t.Fatal("missing vectors")
	}
	for _, v := range vectors {
		t.Run(fmt.Sprint(v.Size), func(t *testing.T) {
			data := make([]byte, v.Size)
			for i := range data {
				data[i] = byte(i % 251)
			}
			ob := make(vectorBuffer, OutboardSize(v.Size))
			mh, err := Outboard(ob, bytes.NewReader(data), v.Size)
			if err != nil || hex.EncodeToString(mh.Digest[:]) != v.Root {
				t.Fatalf("root: %v", err)
			}
			if got := blake3.Sum256(ob); hex.EncodeToString(got[:]) != v.OutboardHash {
				t.Fatal("outboard differs from bao-tree")
			}
			for _, sl := range v.Slices {
				r := Range{sl.Start, sl.End}
				var buf bytes.Buffer
				if err := ExtractSlice(&buf, bytes.NewReader(data), bytes.NewReader(ob), v.Size, r); err != nil {
					t.Fatal(err)
				}
				got := blake3.Sum256(buf.Bytes())
				if buf.Len() != sl.Bytes || hex.EncodeToString(got[:]) != sl.Hash {
					t.Fatalf("slice %v differs from bao-tree", r)
				}
				if err := DecodeSlice(&buf, mh, v.Size, r, Sink{}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

type vectorBuffer []byte

func (b vectorBuffer) WriteAt(p []byte, off int64) (int, error) { return copy(b[off:], p), nil }

func BenchmarkHardening(b *testing.B) {
	slow.Skip(b)
	data := make([]byte, 8<<20)
	for i := range data {
		data[i] = byte(i % 251)
	}
	b.Run("BLAKE3", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		for b.Loop() {
			_ = blake3.Sum256(data)
		}
	})
	b.Run("BaoOutboard16KiB", func(b *testing.B) {
		ob := make(vectorBuffer, OutboardSize(uint64(len(data))))
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		for b.Loop() {
			if _, err := Outboard(ob, bytes.NewReader(data), uint64(len(data))); err != nil {
				b.Fatal(err)
			}
		}
	})
}
