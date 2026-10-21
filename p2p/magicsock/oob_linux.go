//go:build linux

package magicsock

import (
	"encoding/binary"

	"github.com/whyrusleeping/nu/p2p/wire"
	"golang.org/x/sys/unix"
)

// Inner routes never propagate ECN or stale physical source/interface metadata.
// Retain UDP_SEGMENT for a direct write, and split it for reliable relay framing.
// Return a borrowed cmsg view: WriteMsgUDP consumes it synchronously, so neither
// a parsed-message slice nor a copied/rebuilt control buffer is necessary.
func innerOOB(oob []byte) ([]byte, int, error) {
	var kept []byte
	segment := 0
	for len(oob) >= unix.CmsgLen(0) {
		h, body, rest, err := unix.ParseOneSocketControlMessage(oob)
		if err != nil {
			return nil, 0, err
		}
		if h.Level == unix.SOL_UDP && h.Type == unix.UDP_SEGMENT {
			if len(body) != 2 || segment != 0 {
				return nil, 0, wire.ErrFormat
			}
			segment = int(binary.NativeEndian.Uint16(body))
			if segment == 0 {
				return nil, 0, wire.ErrFormat
			}
			kept = oob[:min(unix.CmsgSpace(len(body)), len(oob))]
		}
		oob = rest
	}
	return kept, segment, nil
}
