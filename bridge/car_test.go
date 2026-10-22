package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"testing"
)

func TestCARSourceVerifiesAndLoadsIPFSBlocks(t *testing.T) {
	data := []byte("raw payload")
	sum := sha256.Sum256(data)
	id := IPFSCID{Version: 1, Codec: codecRaw, Hash: hashSHA256, Digest: sum}
	// CARv1 header: {roots:[tag(42, bytes(0 || CID))], version:1}.
	hdr := []byte{0xa2, 0x65, 'r', 'o', 'o', 't', 's', 0x81, 0xd8, 0x2a, 0x58, 0x25, 0}
	hdr = append(hdr, id.Bytes()...)
	hdr = append(hdr, 0x67, 'v', 'e', 'r', 's', 'i', 'o', 'n', 0x01)
	var car bytes.Buffer
	putVarint(&car, uint64(len(hdr)))
	car.Write(hdr)
	block := append(id.Bytes(), data...)
	putVarint(&car, uint64(len(block)))
	car.Write(block)
	src, e := NewCARSource(bytes.NewReader(car.Bytes()))
	if e != nil {
		t.Fatal(e)
	}
	if len(src.Roots) != 1 || src.Roots[0] != id {
		t.Fatalf("roots: %+v", src.Roots)
	}
	got, e := src.GetBlock(context.Background(), id)
	if e != nil || !bytes.Equal(got, data) {
		t.Fatalf("block %q %v", got, e)
	}
	car.Bytes()[car.Len()-1] ^= 0xff
	if _, e = NewCARSource(bytes.NewReader(car.Bytes())); e == nil {
		t.Fatal("accepted tampered block")
	}
}
func putVarint(w *bytes.Buffer, v uint64) {
	var b [10]byte
	n := binary.PutUvarint(b[:], v)
	w.Write(b[:n])
}
