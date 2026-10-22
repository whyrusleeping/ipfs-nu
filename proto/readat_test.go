package proto

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"testing"
)

type countedReaderAt struct {
	io.ReaderAt
	calls int
}

func (r *countedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.calls++
	return r.ReaderAt.ReadAt(p, off)
}

func TestReadAheadAt(t *testing.T) {
	data := make([]byte, 10003)
	rand.NewChaCha8([32]byte{1}).Read(data)
	src := &countedReaderAt{ReaderAt: bytes.NewReader(data)}
	r := newReadAheadAt(src, 4096)
	for off := int64(0); off < 4096; off += 64 {
		got := make([]byte, 64)
		if n, err := r.ReadAt(got, off); err != nil || n != len(got) || !bytes.Equal(got, data[off:off+64]) {
			t.Fatal(off, n, err)
		}
	}
	if src.calls != 1 {
		t.Fatalf("64 adjacent parent reads used %d underlying reads", src.calls)
	}
	// Arbitrary order, cache boundaries, oversized reads and partial/final EOF.
	rng := rand.New(rand.NewPCG(1, 2))
	for range 500 {
		off := rng.Int64N(int64(len(data) + 100))
		got, want := make([]byte, rng.IntN(5000)+1), []byte(nil)
		want = make([]byte, len(got))
		gn, ge := r.ReadAt(got, off)
		wn, we := bytes.NewReader(data).ReadAt(want, off)
		if gn != wn || !errors.Is(ge, we) || !bytes.Equal(got[:gn], want[:wn]) {
			t.Fatalf("off=%d len=%d: got %d %v want %d %v", off, len(got), gn, ge, wn, we)
		}
	}
	if _, err := r.ReadAt(make([]byte, 1), -1); err == nil {
		t.Fatal("negative offset accepted")
	}
}
