package host

import (
	"context"
	"net"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/p2p/wss"
	"github.com/whyrusleeping/nu/p2p/yamux"
)

// transportStream preserves QUIC's half-close contract at the host seam.
// yamux cannot cancel just the read direction on the wire; local CloseRead
// releases the stream after FIN instead of claiming QUIC STOP_SENDING semantics.
type transportStream interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
	CancelRead(quic.StreamErrorCode)
	CancelWrite(quic.StreamErrorCode)
	SetDeadline(time.Time) error
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}
type yamuxStream struct{ *yamux.Stream }

func (s yamuxStream) Close() error { return s.CloseWrite() }
func (s yamuxStream) CancelRead(code quic.StreamErrorCode) {
	if code == 0 {
		_ = s.Stream.Close()
	} else {
		s.Reset()
	}
}
func (s yamuxStream) CancelWrite(quic.StreamErrorCode) { s.Reset() }
func (c *Conn) transportStream(ctx context.Context, open bool) (transportStream, error) {
	if c.tcp == nil {
		if open {
			return c.quic.OpenStreamSync(ctx)
		}
		return c.quic.AcceptStream(ctx)
	}
	var st *yamux.Stream
	var err error
	if open {
		st, err = c.tcp.OpenStream(ctx)
	} else {
		st, err = c.tcp.AcceptStream(ctx)
	}
	if err != nil {
		return nil, err
	}
	return yamuxStream{st}, nil
}

// DialWSS dials only an explicitly admitted relay endpoint. Unlike generic Dial,
// it never obtains proxy/TLS policy from a remote address record. Noise verifies
// the endpoint's PeerID before it is added to the host and protocol router.
func (h *Host) DialWSS(ctx context.Context, addr wire.Multiaddr, opts wss.DialOptions) (*Conn, error) {
	endpoint, id, err := wss.Endpoint(addr)
	if err != nil {
		return nil, err
	}
	if err = h.checkPeer(id); err != nil {
		return nil, err
	}
	if err = h.checkDial(id); err != nil {
		return nil, err
	}
	if c, ok := h.Connection(id); ok {
		return c, nil
	}
	ctx, finish, err := h.beginWSS(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	raw, err := wss.Dial(ctx, endpoint, opts)
	if err != nil {
		h.releaseConnection()
		return nil, err
	}
	return h.secureWSS(ctx, raw, id, true)
}

// AcceptWSS takes ownership of an already upgraded TLS WebSocket. The listener
// must separately bound TCP/TLS/HTTP slots, including slow pre-upgrade clients.
func (h *Host) AcceptWSS(ctx context.Context, raw net.Conn) (*Conn, error) {
	ctx, finish, err := h.beginWSS(ctx)
	if err != nil {
		raw.Close()
		return nil, err
	}
	defer finish()
	c, err := h.secureWSS(ctx, raw, "", false)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	live := h.accepted[:0]
	for _, old := range h.accepted {
		if old.Context().Err() == nil {
			live = append(live, old)
		}
	}
	h.accepted = append(live, c)
	h.mu.Unlock()
	select {
	case h.acceptReady <- struct{}{}:
	default:
	}
	return c, nil
}
func (h *Host) beginWSS(ctx context.Context) (context.Context, func(), error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, nil, net.ErrClosed
	}
	h.wg.Add(1)
	h.mu.Unlock()
	if err := h.reserveConnection(); err != nil {
		h.wg.Done()
		return nil, nil, err
	}
	child, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(h.ctx, cancel)
	return child, func() { stop(); cancel(); h.wg.Done() }, nil
}
func (h *Host) secureWSS(ctx context.Context, raw net.Conn, id wire.PeerID, outbound bool) (*Conn, error) {
	secured, err := wss.Secure(ctx, raw, h.key, id, outbound, yamux.Config{MaxStreams: h.limits.MaxStreamsPerConnection})
	if err != nil {
		h.releaseConnection()
		return nil, err
	}
	context.AfterFunc(secured.Context(), h.releaseConnection)
	c := &Conn{host: h, tcp: secured, peer: secured.PeerID(), key: secured.PublicKey(), outbound: outbound, streams: make(map[*Stream]struct{})}
	c, err = h.registerConnection(c)
	if err != nil {
		secured.Close()
		return nil, err
	}
	return c, nil
}
