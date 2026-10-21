package host

import (
	"context"
	"errors"
	"net"
	"sort"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

type Handler func(*Stream)
type protocolHandler struct {
	handle Handler
	slots  chan struct{}
}

// Handle registers or replaces a protocol handler with a positive concurrency
// limit across all peers. The dispatcher closes streams on handler return;
// handlers may Reset to signal failure and must return when the connection closes.
func (h *Host) Handle(protocol string, maxConcurrent int, handler Handler) error {
	if !wire.ValidProtocol(protocol) || maxConcurrent <= 0 || handler == nil {
		return wire.ErrFormat
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return net.ErrClosed
	}
	if old := h.handlers[protocol]; old != nil && len(old.slots) > 0 {
		return ErrLimit
	}
	h.handlers[protocol] = &protocolHandler{handle: handler, slots: make(chan struct{}, maxConcurrent)}
	return nil
}
func (h *Host) RemoveHandler(protocol string) {
	h.mu.Lock()
	delete(h.handlers, protocol)
	h.mu.Unlock()
}
func (h *Host) Protocols() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.handlers))
	for p := range h.handlers {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// OpenStreamLazy returns immediately after reserving a QUIC stream. Its first
// Write pipelines negotiation with data; Read or Confirm observes any na error.
// The context bounds stream creation, not its lifetime; set a stream deadline
// for request I/O. Close/Reset is always required, including on rejection.
func (h *Host) OpenStreamLazy(ctx context.Context, peer wire.PeerID, protocol string) (*Stream, error) {
	if !wire.ValidProtocol(protocol) {
		return nil, wire.ErrFormat
	}
	c, ok := h.Connection(peer)
	if !ok {
		return nil, ErrNoConnection
	}
	s, err := c.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	s.protocol = protocol
	s.lazy, err = wire.NewLazySelect(s.quic, protocol)
	if err != nil {
		s.Reset()
		return nil, err
	}
	return s, nil
}

// OpenStream eagerly confirms selection; cancellation interrupts negotiation.
func (h *Host) OpenStream(ctx context.Context, peer wire.PeerID, protocol string) (*Stream, error) {
	s, err := h.OpenStreamLazy(ctx, peer, protocol)
	if err != nil {
		return nil, err
	}
	_ = s.SetDeadline(time.Now().Add(5 * time.Second))
	if d, ok := ctx.Deadline(); ok {
		_ = s.SetDeadline(d)
	}
	stop := context.AfterFunc(ctx, func() { s.Reset() })
	err = s.Confirm()
	if !stop() {
		s.Reset()
		return nil, ctx.Err()
	}
	if err != nil {
		s.Reset()
		return nil, err
	}
	_ = s.SetDeadline(time.Time{})
	return s, nil
}
func (c *Conn) routeStreams() {
	defer c.host.wg.Done()
	for {
		s, err := c.AcceptStream(c.Context())
		if err != nil {
			if errors.Is(err, ErrLimit) {
				continue
			}
			return
		}
		c.host.wg.Add(1)
		go func() { defer c.host.wg.Done(); c.host.routeStream(s) }()
	}
}
func (h *Host) routeStream(s *Stream) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(5 * time.Second))
	var selected *protocolHandler
	p, err := wire.Negotiate(s, func(p string) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		handler := h.handlers[p]
		if handler == nil {
			return false
		}
		select {
		case handler.slots <- struct{}{}:
			selected = handler
			return true
		default:
			return false
		}
	})
	if selected != nil {
		defer func() { <-selected.slots }()
	}
	if err != nil {
		s.Reset()
		return
	}
	s.protocol = p
	_ = s.SetDeadline(time.Time{})
	selected.handle(s)
}
