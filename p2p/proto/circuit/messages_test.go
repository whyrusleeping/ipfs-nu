package circuit

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func TestControlVectors(t *testing.T) {
	b, err := os.ReadFile("testdata/messages.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Name, Hex string
		Stop      bool
		Kind      uint64
		Status    Status
		Limit     Limit
	}
	if err = json.Unmarshal(b, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(v.Hex)
			if err != nil {
				t.Fatal(err)
			}
			m, err := decode(raw, v.Stop, MaxMessage)
			if err != nil {
				t.Fatal(err)
			}
			if m.kind != v.Kind || m.status != v.Status || m.limit != v.Limit {
				t.Fatalf("%+v", m)
			}
			if !bytes.Equal(marshal(m, v.Stop), raw) {
				t.Fatal("noncanonical output")
			}
		})
	}
}
func FuzzControlMessages(f *testing.F) {
	for _, b := range [][]byte{{8, 0}, {8, 1}, {8, 2, 40, 100}, {8, 1, 32, 100}, {8, 2, 34, 4, 8, 120, 16, 64}, {8, 0, 8, 1}, {18, 255}, {8, 2, 26, 2, 8, 1}} {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = decode(b, false, MaxMessage); _, _ = decode(b, true, MaxMessage) })
}
