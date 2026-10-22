package bridge

import (
	"encoding/hex"
	"testing"
)

func TestDecodeUnixFSFile(t *testing.T) {
	// UnixFS Data { Type: File, Data:"ok", filesize:2 }
	b, _ := hex.DecodeString("080212026f6b1802")
	d, err := DecodeUnixFSData(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Type != UnixFSFile || string(d.Data) != "ok" || d.Filesize != 2 || !d.HasFilesize {
		t.Fatalf("unexpected: %+v", d)
	}
}
func TestDecodePBNode(t *testing.T) {
	// PBLink{Hash: 0x1220..., Name:"x", Tsize:3}; PBNode{Data:"unixfs", Links:[link]}
	b, _ := hex.DecodeString("0a06756e6978667312290a22" + "1220" + "0000000000000000000000000000000000000000000000000000000000000000" + "1201781803")
	n, err := DecodePBNode(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(n.Data) != "unixfs" || len(n.Links) != 1 || n.Links[0].Name != "x" || n.Links[0].Tsize != 3 {
		t.Fatalf("unexpected: %+v", n)
	}
}
func FuzzDecodePBNode(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x0a, 0x01, 0x00})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = DecodePBNode(b) })
}
func FuzzDecodeUnixFSData(f *testing.F) {
	f.Add([]byte{0x08, 0x02})
	f.Add([]byte{0x08, 0x05})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = DecodeUnixFSData(b) })
}
