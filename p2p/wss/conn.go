// Package wss implements the bounded RFC 6455 binary byte-stream profile used by
// libp2p WSS. It does not authenticate a libp2p peer: negotiate Noise afterwards.
package wss

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"unicode/utf8"
)

const MaxFrame = 1 << 20
const MaxMessage = 4 << 20
const writeFrameSize = 64 << 10

var ErrProtocol = errors.New("wss: invalid or unsupported WebSocket frame")

type frame struct {
	final   bool
	opcode  byte
	payload []byte
}

func readFrame(r io.Reader, server bool) (frame, error) {
	var f frame
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return f, err
	}
	f.final = h[0]&128 != 0
	f.opcode = h[0] & 15
	masked := h[1]&128 != 0
	if h[0]&112 != 0 || masked != server {
		return f, ErrProtocol
	}
	switch f.opcode {
	case 0, 2, 8, 9, 10:
	default:
		return f, ErrProtocol
	}
	n := uint64(h[1] & 127)
	if n == 126 {
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return f, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
		if n < 126 {
			return f, ErrProtocol
		}
	} else if n == 127 {
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return f, err
		}
		n = binary.BigEndian.Uint64(b[:])
		if n < 65536 {
			return f, ErrProtocol
		}
	}
	if n > MaxFrame || (f.opcode >= 8 && (!f.final || n > 125)) {
		return f, ErrProtocol
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return f, err
		}
	}
	f.payload = make([]byte, int(n))
	if _, err := io.ReadFull(r, f.payload); err != nil {
		return f, err
	}
	if masked {
		for i := range f.payload {
			f.payload[i] ^= mask[i%4]
		}
	}
	return f, nil
}

// Conn is a binary WebSocket presented as a net.Conn. One reader and one writer
// may operate concurrently. TLS is provided by Dial or the HTTP server hosting
// Upgrade. Protocol errors are fatal; compression and text are not supported.
type Conn struct {
	net.Conn
	reader          io.Reader
	server          bool
	readMu, writeMu sync.Mutex
	pending         []byte
	fragmented      bool
	messageBytes    int
}

func (c *Conn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(b) == 0 {
		return 0, nil
	}
	for len(c.pending) == 0 {
		f, err := readFrame(c.reader, c.server)
		if err != nil {
			c.Close()
			return 0, fmt.Errorf("wss: frame: %w", err)
		}
		switch f.opcode {
		case 8:
			if len(f.payload) == 1 {
				c.Close()
				return 0, ErrProtocol
			}
			if len(f.payload) >= 2 {
				code := binary.BigEndian.Uint16(f.payload)
				if !validCloseCode(code) || !utf8.Valid(f.payload[2:]) {
					c.Close()
					return 0, ErrProtocol
				}
			}
			// Immediate underlying close avoids blocking a reader on an unresponsive
			// peer's close acknowledgement. EOF is a clean WebSocket shutdown.
			c.Close()
			return 0, io.EOF
		case 9:
			if err := c.writeFrame(10, f.payload); err != nil {
				c.Close()
				return 0, err
			}
			continue
		case 10:
			continue
		case 2:
			if c.fragmented {
				c.Close()
				return 0, ErrProtocol
			}
			c.messageBytes = 0
		case 0:
			if !c.fragmented {
				c.Close()
				return 0, ErrProtocol
			}
		}
		c.messageBytes += len(f.payload)
		if c.messageBytes > MaxMessage {
			c.Close()
			return 0, ErrProtocol
		}
		c.fragmented = !f.final
		c.pending = f.payload
	}
	n := copy(b, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}
func validCloseCode(c uint16) bool {
	return c >= 3000 && c <= 4999 || c >= 1000 && c <= 1014 && c != 1004 && c != 1005 && c != 1006
}
func (c *Conn) writeFrame(op byte, b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	out := []byte{128 | op}
	flag := byte(0)
	if !c.server {
		flag = 128
	}
	switch {
	case len(b) < 126:
		out = append(out, flag|byte(len(b)))
	case len(b) <= 65535:
		out = append(out, flag|126)
		out = binary.BigEndian.AppendUint16(out, uint16(len(b)))
	default:
		out = append(out, flag|127)
		out = binary.BigEndian.AppendUint64(out, uint64(len(b)))
	}
	if !c.server {
		var mask [4]byte
		if _, err := rand.Read(mask[:]); err != nil {
			return err
		}
		out = append(out, mask[:]...)
		for i, v := range b {
			out = append(out, v^mask[i%4])
		}
	} else {
		out = append(out, b...)
	}
	for len(out) > 0 {
		n, err := c.Conn.Write(out)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(out) {
			return io.ErrShortWrite
		}
		out = out[n:]
	}
	return nil
}
func (c *Conn) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		n := min(len(b), writeFrameSize)
		if err := c.writeFrame(2, b[:n]); err != nil {
			c.Close()
			return total, err
		}
		total += n
		b = b[n:]
	}
	return total, nil
}
