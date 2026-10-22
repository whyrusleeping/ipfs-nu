package bridge

import (
	"crypto/sha256"
	"encoding/base32"
	"github.com/mr-tron/base58"
	"testing"
)

func TestIPFSCIDRoundTrip(t *testing.T) {
	data := []byte("dag-pb fixture")
	sum := sha256.Sum256(data)
	c := IPFSCID{Version: 1, Codec: codecDagPB, Hash: hashSHA256, Digest: sum}
	text := c.String()
	got, e := ParseIPFSCID(text)
	if e != nil {
		t.Fatal(e)
	}
	if got != c {
		t.Fatalf("round trip: %#v != %#v", got, c)
	}
	if e = got.Verify(data); e != nil {
		t.Fatal(e)
	}
	if got.Verify([]byte("tampered")) == nil {
		t.Fatal("accepted tampered block")
	}
	v0 := append([]byte{0x12, 32}, sum[:]...)
	v0s := base58.Encode(v0)
	old, e := ParseIPFSCID(v0s)
	if e != nil {
		t.Fatal(e)
	}
	if old.Version != 0 || old.String() != v0s {
		t.Fatalf("CIDv0 round trip %s", old.String())
	}
}
func TestIPFSCIDRejectsUnsupported(t *testing.T) {
	raw := []byte{1, 0x71, 0x12, 32}
	raw = append(raw, make([]byte, 32)...)
	s := "b" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	if _, e := ParseIPFSCID(s); e == nil {
		t.Fatal("accepted dag-cbor")
	}
}
