// Package hardening holds independent format oracles outside Nu's dependency graph.
package hardening

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bluesky-social/indigo/atproto/atdata"
	gocid "github.com/ipfs/go-cid"
	gocar "github.com/ipld/go-car"
	"github.com/ipld/go-car/util"
	"github.com/ipld/go-ipld-prime/codec/dagcbor"
	"github.com/ipld/go-ipld-prime/node/basicnode"
	"github.com/whyrusleeping/nu/core/car"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
)

const vectors = "../../testdata/vectors"

func loadJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vectors, path))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func primeRoundtrip(t *testing.T, data []byte) {
	t.Helper()
	nb := basicnode.Prototype.Any.NewBuilder()
	if err := dagcbor.Decode(nb, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := dagcbor.Encode(nb.Build(), &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		// The generic IPLD model has no negative integer below int64. The pinned
		// refmt/prime decoder wraps CBOR -(2^64) to zero instead of rejecting it.
		// Keep this precise external-oracle limitation visible; Nu must still
		// roundtrip the full DRISL integer range in the caller above.
		if hex.EncodeToString(data) == "3bffffffffffffffff" && bytes.Equal(out.Bytes(), []byte{0}) {
			t.Log("LIMITATION: go-ipld-prime v0.24.0 decodes CBOR -(2^64) as 0; Nu preserves it")
			return
		}
		t.Fatalf("go-ipld-prime encoding differs: got %x want %x", out.Bytes(), data)
	}
}

func TestDASLMatrix(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(vectors, "dasl-testing", "*.json"))
	if err != nil || len(files) != 11 {
		t.Fatal("missing suite", err)
	}
	for _, f := range files {
		var cases []struct {
			Type, Data, Name string
			Tags             []string
		}
		loadJSON(t, filepath.Join("dasl-testing", filepath.Base(f)), &cases)
		applied, other, roundtrips := 0, 0, 0
		for _, c := range cases {
			if !slices.Contains(c.Tags, "dag-cbor") && !slices.Contains(c.Tags, "basic") && !slices.Contains(c.Tags, "dasl-cid") {
				other++
				continue
			}
			applied++
			t.Run(filepath.Base(f)+"/"+c.Name, func(t *testing.T) {
				data, err := hex.DecodeString(c.Data)
				if err != nil {
					t.Fatal(err)
				}
				v, err := drisl.Decode(data)
				switch c.Type {
				case "roundtrip":
					roundtrips++
					if err != nil {
						t.Fatal(err)
					}
					out, err := drisl.Marshal(v)
					if err != nil || !bytes.Equal(out, data) {
						t.Fatal("Nu encoding differs", err)
					}
					primeRoundtrip(t, data)
				case "invalid_in", "invalid_out":
					if err == nil {
						t.Fatal("Nu accepted invalid DRISL")
					}
				default:
					t.Fatal("unknown test type", c.Type)
				}
			})
		}
		t.Logf("MATRIX %s applied=%d other-profile=%d independent-roundtrips=%d", filepath.Base(f), applied, other, roundtrips)
	}
}

func TestIndigoDataModel(t *testing.T) {
	var fixtures []struct {
		JSON json.RawMessage
		CBOR string `json:"cbor_base64"`
		CID  string
	}
	loadJSON(t, "atproto-data-model/data-model-fixtures.json", &fixtures)
	for i, f := range fixtures {
		want, err := base64.RawStdEncoding.DecodeString(f.CBOR)
		if err != nil {
			t.Fatal(err)
		}
		v, err := atdata.UnmarshalJSON(f.JSON)
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		enc, err := atdata.MarshalCBOR(v)
		if err != nil || !bytes.Equal(enc, want) {
			t.Fatalf("%d: indigo JSON encoding differs: %v", i, err)
		}
		v, err = atdata.UnmarshalCBOR(want)
		if err != nil {
			t.Fatal(err)
		}
		enc, err = atdata.MarshalCBOR(v)
		if err != nil || !bytes.Equal(enc, want) {
			t.Fatal("indigo CBOR differs", err)
		}
		nv, err := drisl.FromJSON(f.JSON, drisl.JSONOptions{Atproto: true})
		if err != nil {
			t.Fatal(err)
		}
		nc, nb, err := drisl.Sum(nv)
		if err != nil || nc.String() != f.CID || !bytes.Equal(nb, enc) {
			t.Fatal("Nu/indigo differ", err)
		}
		primeRoundtrip(t, nb)
	}
	for _, kind := range []string{"valid", "invalid"} {
		var cases []struct {
			Note string
			JSON json.RawMessage
		}
		loadJSON(t, "atproto-data-model/data-model-"+kind+".json", &cases)
		for _, c := range cases {
			_, ie := atdata.UnmarshalJSON(c.JSON)
			nv, ne := drisl.FromJSON(c.JSON, drisl.JSONOptions{Atproto: true})
			if ne == nil {
				ne = drisl.ValidateAtproto(nv)
			}
			if (ie == nil) != (kind == "valid") || (ne == nil) != (kind == "valid") {
				t.Errorf("%s %s indigo=%v Nu=%v", kind, c.Note, ie, ne)
			}
		}
		t.Logf("%s: %d cases", kind, len(cases))
	}
	t.Logf("%d byte-exact JSON/CBOR/CID fixtures", len(fixtures))
}

// Upstream dasl-testing has no CAR files at the pinned revision. Exercise both
// implementations and both raw hash profiles explicitly, not as upstream fixtures.
func TestCARBothDirections(t *testing.T) {
	for _, hash := range []cid.Hash{cid.SHA256, cid.BLAKE3} {
		data := []byte("independent CAR payload")
		c, err := cid.SumCID(cid.Raw, hash, data)
		if err != nil {
			t.Fatal(err)
		}
		gc, err := gocid.Cast(c.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		var nu bytes.Buffer
		w, err := car.NewWriter(&nu, car.Header{Roots: []cid.CID{c}})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Put(car.Block{CID: c, Data: data}); err != nil {
			t.Fatal(err)
		}
		r, err := gocar.NewCarReader(bytes.NewReader(nu.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		b, err := r.Next()
		if err != nil || b.Cid() != gc || !bytes.Equal(b.RawData(), data) {
			t.Fatal("upstream read", err)
		}
		if _, err := r.Next(); err != io.EOF {
			t.Fatal("trailing data", err)
		}
		var upstream bytes.Buffer
		if err := gocar.WriteHeader(&gocar.CarHeader{Roots: []gocid.Cid{gc}, Version: 1}, &upstream); err != nil {
			t.Fatal(err)
		}
		if err := util.LdWrite(&upstream, gc.Bytes(), data); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(nu.Bytes(), upstream.Bytes()) {
			t.Fatal("CAR encodings differ")
		}
		nr, err := car.NewReader(bytes.NewReader(upstream.Bytes()), car.Options{})
		if err != nil {
			t.Fatal(err)
		}
		nb, _, err := nr.Next()
		if err != nil || nb.CID != c || !bytes.Equal(nb.Data, data) {
			t.Fatal("Nu read", err)
		}
		bad := bytes.Clone(upstream.Bytes())
		bad[len(bad)-1] ^= 1
		nr, err = car.NewReader(bytes.NewReader(bad), car.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = nr.Next(); err == nil {
			t.Fatal("accepted corrupted block")
		}
	}
}
