package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
)

var ErrNoAddresses = errors.New("host: no peer addresses; provide a link/member address or enable LAN discovery")

type DialFailure struct {
	Address wire.Multiaddr
	Err     error
}
type DialError struct {
	Peer         wire.PeerID
	Failures     []DialFailure
	LookupErrors []error
}

func (e *DialError) Error() string {
	text := fmt.Sprintf("host: all dials to %s failed", e.Peer)
	if len(e.Failures) == 0 {
		text = fmt.Sprintf("%v (%s)", ErrNoAddresses, e.Peer)
	}
	for _, f := range e.Failures {
		text += fmt.Sprintf("; %s: %v", f.Address, f.Err)
	}
	for _, err := range e.LookupErrors {
		text += "; " + err.Error()
	}
	return text
}
func (e *DialError) Unwrap() []error {
	out := append([]error(nil), e.LookupErrors...)
	for _, f := range e.Failures {
		out = append(out, f.Err)
	}
	if len(e.Failures) == 0 {
		out = append(out, ErrNoAddresses)
	}
	return out
}

type SessionConfig struct {
	Lookup lookup.Resolver
	Cache  *lookup.Cache
	// Peers is explicit addressing, and also the initial authorization list when
	// Private is true. Private mode never calls Lookup (including custom sources).
	Peers                                                 map[wire.PeerID][]wire.Multiaddr
	Private                                               bool
	MaxSessions, MaxDials, MaxAddresses, MaxParallelDials int
	Stagger, DialTimeout, ConnectTimeout                  time.Duration
}
type Sessions struct {
	host     *Host
	config   SessionConfig
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
	sessions map[wire.PeerID]*Session
	peers    map[wire.PeerID][]wire.Multiaddr
	dials    chan struct{}
	wg       sync.WaitGroup
	once     sync.Once
}
type Session struct {
	owner   *Sessions
	peer    wire.PeerID
	mu      sync.Mutex
	attempt *connectionAttempt
	closed  bool
}
type connectionAttempt struct {
	done   chan struct{}
	cancel context.CancelFunc
	conn   *Conn
	err    error
}

func NewSessions(h *Host, cfg SessionConfig) (*Sessions, error) {
	if h == nil || h.Context().Err() != nil {
		return nil, net.ErrClosed
	}
	values := []*int{&cfg.MaxSessions, &cfg.MaxDials, &cfg.MaxAddresses, &cfg.MaxParallelDials}
	defaults := []int{128, 16, 32, 4}
	for i, p := range values {
		if *p < 0 {
			return nil, ErrLimit
		}
		if *p == 0 {
			*p = defaults[i]
		}
	}
	if cfg.Stagger < 0 || cfg.DialTimeout < 0 || cfg.ConnectTimeout < 0 {
		return nil, ErrLimit
	}
	if cfg.Stagger == 0 {
		cfg.Stagger = 50 * time.Millisecond
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 2 * time.Second
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 5 * time.Second
	}
	ctx, cancel := context.WithCancel(h.Context())
	m := &Sessions{host: h, config: cfg, ctx: ctx, cancel: cancel, sessions: make(map[wire.PeerID]*Session), peers: make(map[wire.PeerID][]wire.Multiaddr), dials: make(chan struct{}, cfg.MaxDials)}
	for id, addresses := range cfg.Peers {
		if err := m.SetPeer(id, addresses); err != nil {
			cancel()
			return nil, err
		}
	}
	if cfg.Private {
		// Deliberately leave this guard installed on Close, where it denies all:
		// closing a private session manager must never silently make its host public.
		if _, err := h.AddConnectionGater(func(id wire.PeerID) bool {
			m.mu.Lock()
			defer m.mu.Unlock()
			_, ok := m.peers[id]
			return !m.closed && ok
		}); err != nil {
			cancel()
			return nil, err
		}
	}
	return m, nil
}

// SetPeer is an administrative membership/address update, not an untrusted hint.
func (m *Sessions) SetPeer(id wire.PeerID, addresses []wire.Multiaddr) error {
	if _, err := wire.PeerIDFromBytes(id.Bytes()); err != nil {
		return err
	}
	if len(addresses) > m.config.MaxAddresses {
		return ErrLimit
	}
	normalized := make([]wire.Multiaddr, 0, len(addresses))
	for _, a := range addresses {
		addr, err := lookup.NormalizeAddress(id, a)
		if err != nil {
			return err
		}
		normalized = append(normalized, addr)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return net.ErrClosed
	}
	if _, ok := m.peers[id]; !ok && len(m.peers) >= m.config.MaxSessions {
		return ErrLimit
	}
	m.peers[id] = normalized
	return nil
}

// AddHints never authorizes a new private member. Cache persistence errors are
// returned even though already validated hints remain usable in this process.
func (m *Sessions) AddHints(ctx context.Context, hints []lookup.Hint) error {
	for _, h := range hints {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := wire.PeerIDFromBytes(h.Peer.Bytes()); err != nil {
			return err
		}
		if len(h.Addresses) > m.config.MaxAddresses {
			return ErrLimit
		}
		normalized := make([]wire.Multiaddr, 0, len(h.Addresses))
		for _, a := range h.Addresses {
			a, err := lookup.NormalizeAddress(h.Peer, a)
			if err != nil {
				return err
			}
			normalized = append(normalized, a)
		}
		// Recheck membership and commit under one lock: an untrusted hint must
		// never reauthorize a member concurrently revoked by Forget.
		m.mu.Lock()
		old, known := m.peers[h.Peer]
		if m.closed {
			m.mu.Unlock()
			return net.ErrClosed
		}
		if m.config.Private && !known {
			m.mu.Unlock()
			return ErrPeerDenied
		}
		if !known && len(m.peers) >= m.config.MaxSessions {
			m.mu.Unlock()
			return ErrLimit
		}
		addresses := append([]wire.Multiaddr(nil), old...)
		seen := make(map[string]bool)
		for _, a := range old {
			seen[string(a.Bytes())] = true
		}
		for _, a := range normalized {
			if !seen[string(a.Bytes())] {
				addresses = append(addresses, a)
				seen[string(a.Bytes())] = true
			}
		}
		if len(addresses) > m.config.MaxAddresses {
			m.mu.Unlock()
			return ErrLimit
		}
		m.peers[h.Peer] = addresses
		m.mu.Unlock()
		if m.config.Cache != nil {
			if err := m.config.Cache.Remember(ctx, h, "hint", 0.6, 24*time.Hour); err != nil {
				return err
			}
		}
	}
	return nil
}
func (m *Sessions) allowed(id wire.PeerID) error {
	m.mu.Lock()
	closed := m.closed
	_, member := m.peers[id]
	m.mu.Unlock()
	if closed || m.ctx.Err() != nil {
		return net.ErrClosed
	}
	if m.config.Private && !member {
		return fmt.Errorf("%w: %s", ErrPeerDenied, id)
	}
	return m.host.checkPeer(id)
}
func (m *Sessions) Get(id wire.PeerID) (*Session, error) {
	if _, err := wire.PeerIDFromBytes(id.Bytes()); err != nil {
		return nil, err
	}
	if err := m.allowed(id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, net.ErrClosed
	}
	if s := m.sessions[id]; s != nil {
		return s, nil
	}
	if len(m.sessions) >= m.config.MaxSessions {
		return nil, ErrLimit
	}
	s := &Session{owner: m, peer: id}
	m.sessions[id] = s
	return s, nil
}
func (m *Sessions) Connect(ctx context.Context, id wire.PeerID) (*Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	if _, err = s.Connect(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Session) PeerID() wire.PeerID { return s.peer }

// Connect coalesces simultaneous calls. A canceled waiter leaves the shared
// bounded attempt running for other callers; manager Close/Forget cancels it.
func (s *Session) Connect(ctx context.Context) (*Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.owner.allowed(s.peer); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, net.ErrClosed
	}
	if c, ok := s.owner.host.Connection(s.peer); ok {
		s.mu.Unlock()
		return c, nil
	}
	attempt := s.attempt
	if attempt == nil {
		m := s.owner
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			s.mu.Unlock()
			return nil, net.ErrClosed
		}
		m.wg.Add(1)
		m.mu.Unlock()
		child, cancel := context.WithTimeout(m.ctx, m.config.ConnectTimeout)
		attempt = &connectionAttempt{done: make(chan struct{}), cancel: cancel}
		s.attempt = attempt
		go func(a *connectionAttempt) {
			defer m.wg.Done()
			defer cancel()
			a.conn, a.err = s.race(child)
			s.mu.Lock()
			if s.attempt == a {
				s.attempt = nil
			}
			close(a.done)
			s.mu.Unlock()
		}(attempt)
	}
	s.mu.Unlock()
	select {
	case <-attempt.done:
		return attempt.conn, attempt.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.owner.ctx.Done():
		return nil, net.ErrClosed
	}
}

// OpenStream opens an optimistic stream. It may reconnect before opening, but
// never replays application bytes. The request protocol owns verified resume.
func (s *Session) OpenStream(ctx context.Context, protocol string) (*Stream, error) {
	if !wire.ValidProtocol(protocol) {
		return nil, wire.ErrFormat
	}
	for range 2 {
		if _, err := s.Connect(ctx); err != nil {
			if errors.Is(err, ErrNoConnection) {
				continue
			}
			return nil, err
		}
		stream, err := s.owner.host.OpenStreamLazy(ctx, s.peer, protocol)
		if errors.Is(err, ErrNoConnection) || errors.Is(err, net.ErrClosed) {
			continue
		}
		return stream, err
	}
	return nil, ErrNoConnection
}
func (m *Sessions) OpenStream(ctx context.Context, id wire.PeerID, protocol string) (*Stream, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	return s.OpenStream(ctx, protocol)
}

// Forget removes the canonical session and invalidates retained Session handles.
// removeMember also revokes private membership/address authorization.
func (m *Sessions) Forget(id wire.PeerID, removeMember bool) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	if removeMember {
		delete(m.peers, id)
	}
	m.mu.Unlock()
	if s != nil {
		s.mu.Lock()
		s.closed = true
		if s.attempt != nil {
			s.attempt.cancel()
		}
		s.mu.Unlock()
	}
	m.host.Disconnect(id)
	m.host.RecheckConnections()
}
func (m *Sessions) Close() error {
	m.once.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.cancel()
		m.mu.Unlock()
		m.host.RecheckConnections()
		m.wg.Wait()
	})
	return nil
}
