package proto

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
)

// Wire vectors decode and re-encode byte-exactly.
func TestVectors(t *testing.T) {
	raw, err := os.ReadFile("../testdata/vectors/nu-proto/messages.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []struct{ Name, Kind, Hex string }
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		b, _ := hex.DecodeString(v.Hex)
		var out []byte
		var err error
		switch v.Kind {
		case "bytes-request":
			var r BytesRequest
			if r, err = ParseBytesRequest(b); err == nil {
				out, err = r.Marshal()
			}
		case "have-request":
			var qs []HaveQuery
			if qs, err = ParseHaveRequest(b); err == nil {
				out, err = MarshalHaveRequest(qs)
			}
		case "tree-request":
			var r TreeRequest
			if r, err = ParseTreeRequest(b); err == nil {
				out, err = r.Marshal()
			}
		case "tree-frame":
			t := &treeRead{r: bytes.NewReader(b)}
			var tag byte
			var payload []byte
			if tag, payload, err = t.frame(); err == nil && tag == frameEnd {
				_, err = t.trailer(payload)
			}
			out = b
		case "bytes-header":
			var h bytesHeader
			if err = drisl.Unmarshal(b, &h); err == nil {
				out, err = drisl.Marshal(h)
			}
		case "have-response":
			var res []HaveResult
			if _, _, res, err = parseHaveResponse(b, 2); err == nil && (res[0].Held || !res[1].Held) {
				t.Fatalf("%s: %+v", v.Name, res)
			} else if err == nil {
				out, err = drisl.Marshal(haveResponseWire{Have: []byte{2}, Info: []haveInfoWire{haveAnswer(res[1])}})
			}
		}
		if err != nil {
			t.Fatalf("%s: %v", v.Name, err)
		}
		if !bytes.Equal(out, b) {
			t.Fatalf("%s: re-encoded %x", v.Name, out)
		}
	}
}

func FuzzBytesRequest(f *testing.F) {
	mh := cid.Sum(cid.BLAKE3, []byte("nu"))
	for _, r := range []BytesRequest{{MH: mh}, {MH: mh, Ranges: []blob.Range{{Start: 0, End: 64}, {Start: 128, End: 130}}}} {
		b, _ := r.Marshal()
		f.Add(b)
	}
	f.Add([]byte{0xa1, 0x62, 'm', 'h', 0x40})
	f.Add([]byte{0xa0})
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseBytesRequest(b)
		if err != nil {
			return
		}
		enc, err := r.Marshal()
		if err != nil {
			t.Fatalf("valid request does not re-encode: %v", err)
		}
		r2, err := ParseBytesRequest(enc)
		if err != nil || r2.MH != r.MH || len(r2.Ranges) != len(r.Ranges) {
			t.Fatalf("round trip: %+v %v", r2, err)
		}
	})
}

func FuzzHaveRequest(f *testing.F) {
	mh := cid.Sum(cid.BLAKE3, []byte("nu"))
	b, _ := MarshalHaveRequest([]HaveQuery{{MH: cid.Sum(cid.SHA256, nil)}, {MH: mh, Window: &blob.Range{Start: 1, End: 9}}})
	f.Add(b)
	f.Add([]byte{0xa1, 0x64, 'w', 'a', 'n', 't', 0x80})
	f.Fuzz(func(t *testing.T, b []byte) {
		qs, err := ParseHaveRequest(b)
		if err != nil {
			return
		}
		enc, err := MarshalHaveRequest(qs)
		if err != nil {
			t.Fatalf("valid request does not re-encode: %v", err)
		}
		if qs2, err := ParseHaveRequest(enc); err != nil || len(qs2) != len(qs) {
			t.Fatalf("round trip: %v", err)
		}
	})
}

// FuzzHaveResponse covers the client's decoder of untrusted have responses.
func FuzzHaveResponse(f *testing.F) {
	b, _ := drisl.Marshal(haveResponseWire{Have: []byte{1}, Info: []haveInfoWire{haveAnswer(HaveResult{Held: true, Codec: cid.Raw, Size: 1 << 30, Window: blob.Range{Start: 0, End: 100}, Ranges: []blob.Range{{Start: 3, End: 5}}})}})
	f.Add(b, 1)
	f.Fuzz(func(t *testing.T, b []byte, n int) {
		if n < 0 || n > MaxHaveItems {
			return
		}
		_, _, res, err := parseHaveResponse(b, n)
		if err != nil || res == nil {
			return
		}
		for _, r := range res {
			for _, g := range r.Ranges {
				if g.Start >= g.End || g.End > r.Window.End || g.Start < r.Window.Start {
					t.Fatalf("accepted invalid range %v in %v", g, r.Window)
				}
			}
		}
	})
}
