// Package autonat implements the libp2p AutoNAT v2 client and opt-in server over
// literal QUIC-v1 addresses. Results are per-address, per-observer evidence, not
// a NAT classification or permission to advertise an address.
package autonat

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

var (
	ErrPrivateMode = errors.New("autonat: disabled in private mode")
	ErrRefused     = errors.New("autonat: all addresses refused")
	ErrRejected    = errors.New("autonat: request rate limited or rejected")
	ErrUnverified  = errors.New("autonat: no consistent nonce-verified dial back")
	ErrDialData    = errors.New("autonat: dial data cost refused or invalid")
)

// Config defaults to client only. The owner must pass the network's Private
// policy; no AutoNAT protocols are registered in that mode, even with Server set.
type Config struct {
	Server, Private bool
	// AllowPrivateAddresses is only for isolated labs/tests. Unspecified and
	// multicast destinations are always refused. Production must leave this false.
	AllowPrivateAddresses                                  bool
	Timeout, DialTimeout                                   time.Duration // 15s request, 10s dial
	MaxConcurrent                                          int           // 8 inbound and 8 outbound requests
	RequestsPerMinute, PerPeerPerMinute, DialDataPerMinute int           // 60, 12, 12
}

type Reachability uint8

const (
	Unknown Reachability = iota
	Reachable
	Unreachable
)

type Request struct {
	Address      wire.Multiaddr
	SendDialData bool // allow paying up to 100,000 bytes for this candidate
}

type Result struct {
	Address      wire.Multiaddr
	Index        int
	Reachability Reachability
	Server       wire.PeerID
	// Endpoint is the observer's actual request-connection endpoint. The caller
	// should use independent observers, not count multiple IDs at one endpoint.
	Endpoint string
	Checked  time.Time
}

type receipt struct{ local net.Addr }
type pending struct {
	conn *host.Conn
	ch   chan receipt
}
type Service struct {
	host     *host.Host
	cfg      Config
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
	wg       sync.WaitGroup
	once     sync.Once
	pending  map[uint64]pending
	requests chan struct{}
	lim      limiter
}

// New registers the dial-back handler, and the dial-request handler only when
// Server is explicitly enabled. It owns neither h nor address publication.
func New(h *host.Host, cfg Config) (*Service, error) {
	if cfg.Private {
		return nil, ErrPrivateMode
	}
	if h == nil || h.Context().Err() != nil {
		return nil, net.ErrClosed
	}
	if cfg.Timeout < 0 || cfg.DialTimeout < 0 || cfg.MaxConcurrent < 0 || cfg.RequestsPerMinute < 0 || cfg.PerPeerPerMinute < 0 || cfg.DialDataPerMinute < 0 {
		return nil, wire.ErrLimit
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 8
	}
	if cfg.RequestsPerMinute == 0 {
		cfg.RequestsPerMinute = 60
	}
	if cfg.PerPeerPerMinute == 0 {
		cfg.PerPeerPerMinute = 12
	}
	if cfg.DialDataPerMinute == 0 {
		cfg.DialDataPerMinute = 12
	}
	for _, p := range h.Protocols() {
		if p == DialBackProtocol || (cfg.Server && p == DialProtocol) {
			return nil, errors.New("autonat: already registered")
		}
	}
	ctx, cancel := context.WithCancel(h.Context())
	s := &Service{host: h, cfg: cfg, ctx: ctx, cancel: cancel, pending: make(map[uint64]pending), requests: make(chan struct{}, cfg.MaxConcurrent)}
	s.lim.active = make(map[wire.PeerID]bool)
	if err := h.Handle(DialBackProtocol, cfg.MaxConcurrent, s.handleDialBack); err != nil {
		cancel()
		return nil, err
	}
	if cfg.Server {
		if err := h.Handle(DialProtocol, cfg.MaxConcurrent, s.handleRequest); err != nil {
			h.RemoveHandler(DialBackProtocol)
			cancel()
			return nil, err
		}
	}
	return s, nil
}
func (s *Service) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return false
	}
	s.wg.Add(1)
	return true
}
func (s *Service) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.cancel()
		s.mu.Unlock()
		s.host.RemoveHandler(DialBackProtocol)
		if s.cfg.Server {
			s.host.RemoveHandler(DialProtocol)
		}
		s.wg.Wait()
	})
	return nil
}

// bind bounds the entire operation, including stream I/O, by service lifetime
// and the caller's context. Cancellation resets the stream, not its connection.
func (s *Service) bind(ctx context.Context, stream *host.Stream) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	stopService := context.AfterFunc(s.ctx, cancel)
	stopIO := context.AfterFunc(ctx, stream.Reset)
	deadline, _ := ctx.Deadline()
	_ = stream.SetDeadline(deadline)
	return ctx, func() { stopIO(); stopService(); cancel() }
}
