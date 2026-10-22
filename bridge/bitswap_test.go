package bridge

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func TestBitswapWantAndPayload(t *testing.T) {
	data := []byte("bitswap payload")
	id := IPFSCID{Version: 1, Codec: codecDagPB, Hash: hashSHA256, Digest: sha256.Sum256(data)}
	ent := pbBytes(nil, 1, id.Bytes())
	ent = pbVarint(ent, 2, 1)
	ent = pbVarint(ent, 4, 0)
	ent = pbVarint(ent, 5, 1)
	want := pbBytes(nil, 1, ent)
	want = pbVarint(want, 2, 1)
	msg := pbBytes(nil, 1, want)
	f, w, v, rest, e := pbField(msg)
	if e != nil || f != 1 || w != 2 || len(rest) != 0 {
		t.Fatalf("wantlist parse %d %d %v", f, w, e)
	}
	ff, ww, vv, _, e := pbField(v)
	if e != nil || ff != 1 || ww != 2 {
		t.Fatal("wantlist entry absent")
	}
	f, w, block, _, e := pbField(vv)
	if e != nil || f != 1 || w != 2 || !bytes.Equal(block, id.Bytes()) {
		t.Fatalf("want CID mismatch %x %v", block, e)
	}
	prefix := appendUvarint(nil, 1)
	prefix = appendUvarint(prefix, codecDagPB)
	prefix = appendUvarint(prefix, hashSHA256)
	prefix = appendUvarint(prefix, 32)
	blockMsg := pbBytes(nil, 1, prefix)
	blockMsg = pbBytes(blockMsg, 2, data)
	payload := pbBytes(nil, 3, blockMsg)
	got, c, e := parseBitswapPayload(payload)
	if e != nil || c != id || !bytes.Equal(got, data) {
		t.Fatalf("payload %s %q %v", c.String(), got, e)
	}
	if _, _, e = parseBitswapPayload(pbBytes(nil, 3, pbBytes(nil, 2, []byte("bad")))); e == nil {
		t.Fatal("accepted unbound block payload")
	}
}
