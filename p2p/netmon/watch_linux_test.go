package netmon

import (
	"encoding/binary"
	"testing"

	"golang.org/x/sys/unix"
)

func FuzzNetlink(f *testing.F) {
	f.Add([]byte{})
	b := make([]byte, 16)
	binary.NativeEndian.PutUint32(b, 16)
	binary.NativeEndian.PutUint16(b[4:], unix.RTM_NEWADDR)
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = parseNetlink(b, 64<<10) })
}
