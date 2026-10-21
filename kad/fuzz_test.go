package kad

import "testing"

func FuzzDecode(f *testing.F) {
	f.Add([]byte{8, 3, 18, 4, 0x1e, 32, 1, 2})
	f.Add([]byte{8, 4})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = Decode(b) })
}
