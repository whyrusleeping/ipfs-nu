package host

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/p2p/wss"
)

// Conn is an authenticated QUIC-v1 or WSS/Noise/yamux connection. Raw stream
// methods are transport primitives; applications should use protocol routing.
type Conn struct {
	host     *Host
	quic     *quic.Conn
	tcp      *wss.SecureConn
	peer     wire.PeerID
	key      wire.PublicKey
	outbound bool
	streams  map[*Stream]struct{} // protected by host.mu
	datagramState
}

func (c *Conn) PeerID() wire.PeerID       { return c.peer }
func (c *Conn) PublicKey() wire.PublicKey { return c.key }
func (c *Conn) Context() context.Context {
	if c.tcp != nil {
		return c.tcp.Context()
	}
	return c.quic.Context()
}
func (c *Conn) LocalAddr() net.Addr {
	if c.tcp != nil {
		return c.tcp.LocalAddr()
	}
	return c.quic.LocalAddr()
}
func (c *Conn) RemoteAddr() net.Addr {
	if c.tcp != nil {
		return c.tcp.RemoteAddr()
	}
	return c.quic.RemoteAddr()
}
func (c *Conn) Close() error {
	if c.tcp != nil {
		return c.tcp.Close()
	}
	return c.quic.CloseWithError(0, "closed")
}

// Transport identifies the authenticated outer transport, for diagnostics.
func (c *Conn) Transport() string {
	if c.tcp != nil {
		return "wss"
	}
	return "quic-v1"
}

// ExportKeyingMaterial derives protocol-specific key material from the already
// authenticated inner TLS session. Callers choose a domain-separated label;
// this is not permission to probe destinations (0017 owns that policy).
func (c *Conn) ExportKeyingMaterial(label string, context []byte, length int) ([]byte, error) {
	if length < 1 || length > 1024 {
		return nil, wire.ErrLimit
	}
	if c.quic == nil {
		return nil, errors.New("host: TLS exporter requires inner QUIC, not outer WSS")
	}
	state := c.quic.ConnectionState().TLS
	return state.ExportKeyingMaterial(label, context, length)
}
func (c *Conn) reserveStream() (*Stream, error) {
	h := c.host
	for {
		version, err := h.checkPeerVersion(c.peer)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		if version == h.gateVersion {
			break
		}
		h.mu.Unlock()
	}
	defer h.mu.Unlock()
	if h.closed || c.Context().Err() != nil {
		return nil, net.ErrClosed
	}
	if h.totalStreams >= h.limits.MaxStreams || h.peerStreams[c.peer] >= h.limits.MaxStreamsPerPeer || len(c.streams) >= h.limits.MaxStreamsPerConnection {
		return nil, ErrLimit
	}
	s := &Stream{conn: c}
	c.streams[s] = struct{}{}
	h.totalStreams++
	h.peerStreams[c.peer]++
	return s, nil
}
func (c *Conn) OpenStream(ctx context.Context) (*Stream, error) {
	s, err := c.reserveStream()
	if err != nil {
		return nil, err
	}
	q, err := c.transportStream(ctx, true)
	if err != nil {
		s.release()
		return nil, err
	}
	s.mu.Lock()
	s.quic = q
	closed := s.closed
	s.mu.Unlock()
	if closed {
		q.CancelRead(0x100)
		q.CancelWrite(0x100)
		return nil, net.ErrClosed
	}
	return s, nil
}
func (c *Conn) AcceptStream(ctx context.Context) (*Stream, error) {
	q, err := c.transportStream(ctx, false)
	if err != nil {
		return nil, err
	}
	s, err := c.reserveStream()
	if err != nil {
		q.CancelRead(0x100)
		q.CancelWrite(0x100)
		return nil, err
	}
	s.mu.Lock()
	s.quic = q
	closed := s.closed
	s.mu.Unlock()
	if closed {
		q.CancelRead(0x100)
		q.CancelWrite(0x100)
		return nil, net.ErrClosed
	}
	return s, nil
}

// Stream is a bounded bidirectional stream. Close closes both sides and frees
// its local resource slot; CloseWrite sends FIN while leaving reads available.
// Always Close or Reset, including after EOF. Reset aborts both directions.
type Stream struct {
	conn     *Conn
	mu       sync.Mutex
	quic     transportStream
	closed   bool
	lazy     *wire.LazySelect
	protocol string
	once     sync.Once
}

func (s *Stream) Conn() *Conn { return s.conn }
func (s *Stream) Read(b []byte) (int, error) {
	if s.lazy != nil {
		return s.lazy.Read(b)
	}
	return s.quic.Read(b)
}
func (s *Stream) Write(b []byte) (int, error) {
	if s.lazy != nil {
		return s.lazy.Write(b)
	}
	return s.quic.Write(b)
}
func (s *Stream) Protocol() string { return s.protocol }
func (s *Stream) Confirm() error {
	if s.lazy != nil {
		return s.lazy.Confirm()
	}
	return nil
}
func (s *Stream) SetDeadline(t time.Time) error      { return s.quic.SetDeadline(t) }
func (s *Stream) SetReadDeadline(t time.Time) error  { return s.quic.SetReadDeadline(t) }
func (s *Stream) SetWriteDeadline(t time.Time) error { return s.quic.SetWriteDeadline(t) }
func (s *Stream) CloseWrite() error {
	if s.lazy != nil {
		if _, err := s.lazy.Write(nil); err != nil {
			return err
		}
	}
	return s.quic.Close()
}
func (s *Stream) CloseRead() { s.quic.CancelRead(0) }
func (s *Stream) release() {
	s.once.Do(func() {
		h := s.conn.host
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(s.conn.streams, s)
		h.totalStreams--
		h.peerStreams[s.conn.peer]--
		if h.peerStreams[s.conn.peer] == 0 {
			delete(h.peerStreams, s.conn.peer)
		}
	})
}
func (s *Stream) Close() error {
	s.mu.Lock()
	s.closed = true
	var err error
	if s.quic != nil {
		err = s.quic.Close()
		s.quic.CancelRead(0)
	}
	s.mu.Unlock()
	s.release()
	return err
}
func (s *Stream) Reset() {
	s.mu.Lock()
	s.closed = true
	if s.quic != nil {
		s.quic.CancelRead(0x100)
		s.quic.CancelWrite(0x100)
	}
	s.mu.Unlock()
	s.release()
}
