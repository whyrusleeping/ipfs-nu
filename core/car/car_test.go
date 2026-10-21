package car

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
)

func sample(t testing.TB) ([]byte, cid.CID, cid.CID, cid.CID) {
	leaf := []byte("leaf")
	lc, _ := cid.SumCID(cid.Raw, cid.BLAKE3, leaf)
	rc, root, err := drisl.Sum(map[string]any{"l": lc})
	if err != nil {
		t.Fatal(err)
	}
	stray := []byte("stray")
	sc, _ := cid.SumCID(cid.Raw, cid.SHA256, stray)
	var buf bytes.Buffer
	w, err := NewWriter(&buf, Header{Roots: []cid.CID{rc}, Extra: map[string]any{"io.ipfs.nu": map[string]any{"v": int64(1)}}})
	if err != nil {
		t.Fatal(err)
	}
	// Arbitrary order with a duplicate and an unreferenced block.
	for _, b := range []Block{{lc, leaf}, {rc, root}, {sc, stray}, {lc, leaf}} {
		if err := w.Put(b); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes(), rc, lc, sc
}

func TestReadVerify(t *testing.T) {
	data, rc, lc, sc := sample(t)
	r, err := NewReader(bytes.NewReader(data), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if h := r.Header(); len(h.Roots) != 1 || h.Roots[0] != rc || h.Extra["io.ipfs.nu"] == nil {
		t.Fatalf("header %+v", h)
	}
	var got []cid.CID
	dups := 0
	for {
		b, dup, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if dup {
			dups++
		}
		got = append(got, b.CID)
	}
	if len(got) != 4 || dups != 1 || got[0] != lc {
		t.Fatalf("blocks %v dups %d", got, dups)
	}
	if u := r.Unreferenced(); len(u) != 1 || u[0] != sc {
		t.Fatalf("unreferenced %v", u)
	}

	// A flipped byte in block data fails verification.
	bad := bytes.Clone(data)
	i := bytes.Index(bad, []byte("leaf"))
	bad[i] ^= 1
	r, _ = NewReader(bytes.NewReader(bad), Options{})
	if _, _, err := r.Next(); !errors.Is(err, cid.ErrMismatch) {
		t.Fatalf("corrupt block: %v", err)
	}
	// Truncation is an error, not EOF.
	r, _ = NewReader(bytes.NewReader(data[:len(data)-2]), Options{})
	for err == nil || err == cid.ErrMismatch {
		_, _, err = r.Next()
	}
	if err == io.EOF {
		t.Fatal("truncated CAR read as complete")
	}
}

func TestRejectCARv2(t *testing.T) {
	pragma, _ := hex.DecodeString("0aa16776657273696f6e02")
	if _, err := NewReader(bytes.NewReader(append(pragma, make([]byte, 40)...)), Options{}); err != ErrCARv2 {
		t.Fatalf("CARv2: %v", err)
	}
}

func TestPreOrder(t *testing.T) {
	a, _ := cid.SumCID(cid.Raw, cid.BLAKE3, []byte("a"))
	b, _ := cid.SumCID(cid.Raw, cid.BLAKE3, []byte("b"))
	mid, midb, _ := drisl.Sum([]any{b, a})
	top, topb, _ := drisl.Sum([]any{mid, a})
	blocks := map[cid.CID][]byte{a: []byte("a"), b: []byte("b"), mid: midb, top: topb}
	var order []cid.CID
	err := PreOrder([]cid.CID{top}, func(c cid.CID) ([]byte, error) { return blocks[c], nil }, DRISLLinks, func(bl Block) error {
		order = append(order, bl.CID)
		return nil
	})
	if err != nil || len(order) != 4 || order[0] != top || order[1] != mid || order[2] != b || order[3] != a {
		t.Fatalf("order %v %v", order, err)
	}
}

func FuzzReader(f *testing.F) {
	data, _, _, _ := sample(f)
	f.Add(data)
	f.Add(data[:20])
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := NewReader(bytes.NewReader(b), Options{MaxBlockSize: 1 << 16})
		if err != nil {
			return
		}
		for range 1000 {
			blk, _, err := r.Next()
			if err != nil {
				return
			}
			if blk.CID.Verify(blk.Data) != nil {
				t.Fatal("unverified block returned")
			}
		}
	})
}
