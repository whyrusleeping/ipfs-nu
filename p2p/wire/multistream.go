package wire

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/whyrusleeping/nu/core/varint"
)

const MultistreamHeader = "/multistream/1.0.0"
const MaxProtocolBytes = 1024

var ErrNotSupported = errors.New("wire: protocol not supported")

type byteReader struct{ io.Reader }

func (r byteReader) ReadByte() (byte, error) {
	var b [1]byte
	_, err := io.ReadFull(r.Reader, b[:])
	return b[0], err
}

// ReadMessage reads one minimal uvarint-length-prefixed message, bounding the
// allocation before reading its body. It never reads ahead into the next message.
func ReadMessage(r io.Reader, maxBytes int) ([]byte, error) {
	n, err := varint.Read(byteReader{r})
	if err != nil {
		return nil, err
	}
	if maxBytes < 0 || n > uint64(maxBytes) {
		return nil, ErrLimit
	}
	b := make([]byte, int(n))
	_, err = io.ReadFull(r, b)
	return b, err
}
func WriteMessage(w io.Writer, b []byte, maxBytes int) error {
	if maxBytes < 0 || len(b) > maxBytes {
		return ErrLimit
	}
	if err := writeAll(w, varint.Append(nil, uint64(len(b)))); err != nil {
		return err
	}
	return writeAll(w, b)
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

// ReadProtocol reads one newline-terminated multistream token. The bound includes
// the newline. Tokens cannot contain embedded newlines, NUL or carriage returns.
func ReadProtocol(r io.Reader, maxBytes int) (string, error) {
	b, err := ReadMessage(r, maxBytes)
	if err != nil {
		return "", err
	}
	if len(b) < 2 || !utf8.Valid(b) || b[len(b)-1] != '\n' || bytes.ContainsAny(b[:len(b)-1], "\n\r\x00") {
		return "", ErrFormat
	}
	return string(b[:len(b)-1]), nil
}
func protocolFrame(p string) []byte {
	return append(varint.Append(nil, uint64(len(p)+1)), append([]byte(p), '\n')...)
}
func ValidProtocol(p string) bool {
	return utf8.ValidString(p) && strings.HasPrefix(p, "/") && len(p)+1 <= MaxProtocolBytes && !strings.ContainsAny(p, "\r\n\x00") && p != MultistreamHeader
}

// Negotiate is the multistream-select listener. accept is called for proposals;
// it may reserve a handler slot before returning true. Up to 16 proposals are
// accepted per stream, preventing an unbounded negotiation loop.
func Negotiate(rw io.ReadWriter, accept func(string) bool) (string, error) {
	header, err := ReadProtocol(rw, MaxProtocolBytes)
	if err != nil {
		return "", err
	}
	if header != MultistreamHeader {
		return "", ErrFormat
	}
	if err := writeAll(rw, protocolFrame(MultistreamHeader)); err != nil {
		return "", err
	}
	for range 16 {
		p, err := ReadProtocol(rw, MaxProtocolBytes)
		if err != nil {
			return "", err
		}
		if ValidProtocol(p) && accept(p) {
			if err := writeAll(rw, protocolFrame(p)); err != nil {
				return p, err
			}
			return p, nil
		}
		if err := writeAll(rw, protocolFrame("na")); err != nil {
			return "", err
		}
	}
	return "", ErrLimit
}

// LazySelect pipelines header, proposal and the first Write in one write. The
// first Read (or Confirm) checks both acknowledgements before exposing payload.
// One reader and one writer may operate concurrently, as on a QUIC stream.
// A rejection may arrive after application bytes were sent; callers must not
// retry a non-idempotent request without an application-level acknowledgement.
type LazySelect struct {
	rw        io.ReadWriter
	protocol  string
	mu        sync.Mutex
	sent      bool
	err       error
	confirmed sync.Once
}

func NewLazySelect(rw io.ReadWriter, p string) (*LazySelect, error) {
	if !ValidProtocol(p) {
		return nil, ErrFormat
	}
	return &LazySelect{rw: rw, protocol: p}, nil
}
func (s *LazySelect) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	if !s.sent {
		all := append(protocolFrame(MultistreamHeader), protocolFrame(s.protocol)...)
		all = append(all, b...)
		s.err = writeAll(s.rw, all)
		s.sent = true
		if s.err != nil {
			return 0, s.err
		}
		return len(b), nil
	}
	return s.rw.Write(b)
}
func (s *LazySelect) Confirm() error {
	s.confirmed.Do(func() {
		s.mu.Lock()
		if !s.sent {
			s.err = writeAll(s.rw, append(protocolFrame(MultistreamHeader), protocolFrame(s.protocol)...))
			s.sent = true
		}
		err := s.err
		s.mu.Unlock()
		if err == nil {
			var header, p string
			header, err = ReadProtocol(s.rw, MaxProtocolBytes)
			if err == nil && header != MultistreamHeader {
				err = ErrFormat
			}
			if err == nil {
				p, err = ReadProtocol(s.rw, MaxProtocolBytes)
			}
			if err == nil {
				if p == "na" {
					err = ErrNotSupported
				} else if p != s.protocol {
					err = ErrFormat
				}
			}
		}
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
func (s *LazySelect) Read(b []byte) (int, error) {
	if err := s.Confirm(); err != nil {
		return 0, err
	}
	return s.rw.Read(b)
}

// Select eagerly confirms a pipelined header/proposal (one negotiation RTT).
func Select(rw io.ReadWriter, p string) error {
	s, err := NewLazySelect(rw, p)
	if err != nil {
		return err
	}
	return s.Confirm()
}
