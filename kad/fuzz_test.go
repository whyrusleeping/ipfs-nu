package kad

import "testing"

func FuzzDecode(f *testing.F) {
	f.Add([]byte{8, 3, 18, 4, 0x1e, 32, 1, 2})
	f.Add([]byte{8, 4})
	f.Add([]byte{})
	// GET_PROVIDERS with a provider entry {id: 2 bytes, addr} and an unknown field.
	f.Add([]byte{8, 3, 74, 6, 10, 2, 0, 1, 18, 0, 90, 1, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = Decode(b)
		m, err := DecodeResponse(b)
		if err == nil && (len(m.Providers) > MaxProviders || len(m.Closer) > MaxPeers) {
			t.Fatalf("caps exceeded: %d providers, %d closer", len(m.Providers), len(m.Closer))
		}
	})
}
