package httpsrc

import (
	"fmt"
	"testing"
)

// Only seeds run in the fast suite; fuzz campaigns belong to the hardening exit.
func FuzzContentRange(f *testing.F) {
	for _, s := range []string{"bytes 3-5/6", "bytes 0-0/1", "bytes */6", "bytes 5-3/6", "bytes 0-1/9223372036854775808"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		start, end, total, ok := contentRange(s, 128)
		if !ok {
			return
		}
		if len(s) > 128 || start < 0 || end < start || total <= end {
			t.Fatal("invalid accepted range")
		}
		a, b, c, valid := contentRange(fmt.Sprintf("bytes %d-%d/%d", start, end, total), 128)
		if !valid || a != start || b != end || c != total {
			t.Fatal("range failed round trip")
		}
	})
}
