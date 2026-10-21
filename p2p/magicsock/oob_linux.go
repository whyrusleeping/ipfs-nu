//go:build linux

package magicsock

import (
	"encoding/binary"

	"github.com/whyrusleeping/nu/p2p/wire"
	"golang.org/x/sys/unix"
)

// Inner routes never propagate ECN or stale physical source/interface metadata.
// Retain UDP_SEGMENT for a direct write, and split it for reliable relay framing.
func innerOOB(oob []byte) ([]byte, int, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, 0, err
	}
	var kept []byte
	segment := 0
	off := 0
	for _, m := range msgs {
		space := unix.CmsgSpace(len(m.Data))
		if m.Header.Level == unix.SOL_UDP && m.Header.Type == unix.UDP_SEGMENT {
			if len(m.Data) != 2 || segment != 0 {
				return nil, 0, wire.ErrFormat
			}
			segment = int(binary.NativeEndian.Uint16(m.Data))
			if segment == 0 {
				return nil, 0, wire.ErrFormat
			}
			kept = append(kept, oob[off:min(off+space, len(oob))]...)
		}
		off += space
	}
	return kept, segment, nil
}
