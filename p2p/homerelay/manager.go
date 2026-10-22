// Package homerelay owns a public node's home/backup circuit-v2 reservations.
// It does not accept qpath circuits or recover active peer paths; those belong to
// the connectivity owner. No relay is inferred from an untrusted peer record.
package homerelay

import (
	"context"
	"errors"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
)

type Config struct {
	Host   *host.Host
	Client *circuit.Client
	// Relays is an explicit operator-configured/known list, at most 16 literal
	// /ip4|ip6/.../udp/.../quic-v1/p2p/<relay> endpoints, one per relay PeerID.
	// This is an authorization list, never extended by remote identify messages.
	Relays []wire.Multiaddr
	Ping   func(context.Context, wire.PeerID) (time.Duration, error)
	// Disabled is mandatory for private/isolated owners. It prevents all network
	// activity, including after SetNeeded(true) or Renew. There are no defaults.
	Disabled                               bool
	Timeout, HealthInterval, RetryInterval time.Duration
	// Changed signals publication work; it must be quick and concurrency-safe.
	// No manager lock is held. Typically call identify.AddressesChanged.
	Changed func()
}

type RelayState struct {
	Peer        wire.PeerID
	Address     wire.Multiaddr
	RTT         time.Duration
	Expires     time.Time
	LastSuccess time.Time
	HeldSince   time.Time // start of the current uninterrupted reservation; zero when not held
	LastError   string
}

type State struct {
	Disabled, Needed bool
	Home, Backup     wire.PeerID
	Relays           []RelayState
	Addresses        []wire.Multiaddr
}

type candidate struct {
	state            RelayState
	conn             *host.Conn
	renewAt, retryAt time.Time
	failures         int
	held             bool
}

type renewRequest struct{ done chan error }

type Manager struct {
	cfg       Config
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	needed    atomic.Bool
	wake      chan struct{}
	renew     chan renewRequest
	mu        sync.Mutex
	state     State
	liveConns map[wire.PeerID]*host.Conn
	changed   chan struct{}
	once      sync.Once
	// Only run/reconcile accesses candidates and the chosen home/backup order.
	candidates []*candidate
	chosen     []*candidate
}

func New(ctx context.Context, cfg Config) (*Manager, error) {
	if cfg.Timeout < 0 || cfg.HealthInterval < 0 || cfg.RetryInterval < 0 || len(cfg.Relays) > 16 {
		return nil, wire.ErrLimit
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.HealthInterval == 0 {
		cfg.HealthInterval = 5 * time.Second
	}
	if cfg.RetryInterval == 0 {
		cfg.RetryInterval = time.Second
	}
	if !cfg.Disabled && (cfg.Host == nil || cfg.Client == nil || cfg.Ping == nil) {
		return nil, errors.New("homerelay: host, circuit client and latency probe required")
	}
	child, cancel := context.WithCancel(ctx)
	m := &Manager{cfg: cfg, ctx: child, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), renew: make(chan renewRequest), changed: make(chan struct{})}
	seen := map[wire.PeerID]bool{}
	for _, addr := range cfg.Relays {
		id, err := circuit.RelayPeer(addr)
		if err != nil {
			cancel()
			return nil, err
		}
		if seen[id] {
			cancel()
			return nil, errors.New("homerelay: duplicate relay PeerID")
		}
		seen[id] = true
		if cfg.Host != nil && (id == cfg.Host.ID() || cfg.Host.IsLocalOnlyMultiaddr(addr)) {
			cancel()
			return nil, wire.ErrFormat
		}
		m.candidates = append(m.candidates, &candidate{state: RelayState{Peer: id, Address: addr}})
	}
	m.needed.Store(true) // unknown reachability is not evidence of public reachability
	m.publish()
	if cfg.Disabled {
		close(m.done)
	} else {
		go m.run()
	}
	return m, nil
}

// SetNeeded accepts a reachability result from the connectivity owner. Unknown/
// private reachability should retain reservations. Only a confirmed public result
// disables them. Disabled private/isolated managers cannot be enabled this way.
func (m *Manager) SetNeeded(needed bool) {
	m.needed.Store(needed)
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Renew serializes an early renewal with ordinary health/TTL recovery (D12).
// The caller's context bounds waiting, not the shared recovery operation, which
// is independently bounded by Config.Timeout for each relay request.
func (m *Manager) Renew(ctx context.Context) error {
	if m.cfg.Disabled {
		return nil
	}
	req := renewRequest{done: make(chan error, 1)}
	select {
	case m.renew <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-m.done:
		return net.ErrClosed
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-m.done:
		return net.ErrClosed
	}
}

func (m *Manager) Close() error {
	m.once.Do(func() { m.cancel(); <-m.done })
	return nil
}

func (m *Manager) Snapshot() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state
	s.Relays = append([]RelayState(nil), s.Relays...)
	s.Addresses = append([]wire.Multiaddr(nil), s.Addresses...)
	// Expiration and connection death can precede the loop's next notification.
	// Never advertise a route that is already known dead while recovery is busy.
	s.Addresses = nil
	if !m.needed.Load() || m.cfg.Disabled || m.ctx.Err() != nil {
		return s
	}
	for _, id := range []wire.PeerID{s.Home, s.Backup} {
		for _, r := range s.Relays {
			if r.Peer != id {
				continue
			}
			conn, ok := m.cfg.Host.Connection(id)
			if ok && conn == m.liveConns[id] && conn.Context().Err() == nil && r.Expires.After(time.Now()) {
				addr, err := circuit.Address(r.Address, m.cfg.Host.ID())
				if err == nil {
					s.Addresses = append(s.Addresses, addr)
				}
			}
		}
	}
	return s
}

func (m *Manager) Addresses() []wire.Multiaddr { return m.Snapshot().Addresses }

// Wait supports owner diagnostics and short integration tests without polling.
func (m *Manager) Wait(ctx context.Context, ready func(State) bool) (State, error) {
	for {
		m.mu.Lock()
		changed := m.changed
		m.mu.Unlock()
		state := m.Snapshot()
		if ready(state) {
			return state, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return State{}, ctx.Err()
		case <-m.done:
			return State{}, net.ErrClosed
		}
	}
}

func (m *Manager) publish() {
	s := State{Disabled: m.cfg.Disabled, Needed: m.needed.Load() && !m.cfg.Disabled}
	for _, c := range m.candidates {
		s.Relays = append(s.Relays, c.state)
	}
	if len(m.chosen) > 0 {
		s.Home = m.chosen[0].state.Peer
	}
	if len(m.chosen) > 1 {
		s.Backup = m.chosen[1].state.Peer
	}
	m.mu.Lock()
	publicationChanged := m.state.Home != s.Home || m.state.Backup != s.Backup || m.state.Needed != s.Needed
	m.state = s
	m.liveConns = make(map[wire.PeerID]*host.Conn, len(m.chosen))
	for _, c := range m.chosen {
		if c.held {
			m.liveConns[c.state.Peer] = c.conn
		}
	}
	close(m.changed)
	m.changed = make(chan struct{})
	m.mu.Unlock()
	if publicationChanged && m.cfg.Changed != nil {
		m.cfg.Changed()
	}
}

func (m *Manager) run() {
	defer close(m.done)
	stop := context.AfterFunc(m.cfg.Host.Context(), m.cancel)
	defer stop()
	changes, unwatch := m.cfg.Host.WatchChanges()
	defer unwatch()
	defer func() {
		for _, c := range m.chosen {
			m.cfg.Client.DropReservation(c.state.Peer)
			c.held = false
			c.state.Expires = time.Time{}
			c.state.HeldSince = time.Time{}
		}
		m.chosen = nil
		m.publish()
	}()
	nextHealth := time.Now()
	for m.ctx.Err() == nil {
		health := !time.Now().Before(nextHealth)
		if health {
			nextHealth = time.Now().Add(m.cfg.HealthInterval)
		}
		_ = m.reconcile(false, health)
		delay := time.Until(nextHealth)
		for _, c := range m.candidates {
			next := c.retryAt
			if c.held {
				next = c.renewAt
			}
			if !c.held && (len(m.chosen) >= 2 || !m.needed.Load()) {
				continue
			}
			if !next.IsZero() && time.Until(next) < delay {
				delay = time.Until(next)
			}
		}
		timer := time.NewTimer(max(delay, time.Millisecond))
		select {
		case <-m.ctx.Done():
		case <-m.wake:
		case <-changes:
		case req := <-m.renew:
			req.done <- m.reconcile(true, false)
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (m *Manager) failed(c *candidate, err error) {
	m.cfg.Client.DropReservation(c.state.Peer)
	c.held = false
	c.conn = nil
	c.state.Expires = time.Time{}
	c.state.HeldSince = time.Time{}
	c.state.LastError = err.Error()
	c.failures = min(c.failures+1, 7)
	delay := min(m.cfg.RetryInterval, time.Minute)
	for i := 1; i < c.failures; i++ {
		delay = min(delay*2, time.Minute)
	}
	c.retryAt = time.Now().Add(delay)
}

func (m *Manager) reserve(c *candidate) error {
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.Timeout)
	defer cancel()
	r, err := m.cfg.Client.Reserve(ctx, c.state.Peer)
	if err != nil {
		m.failed(c, err)
		return err
	}
	// A reservation is tied to this exact live connection, not just a PeerID.
	conn, ok := m.cfg.Host.Connection(c.state.Peer)
	if !ok || conn != c.conn {
		err = net.ErrClosed
		m.failed(c, err)
		return err
	}
	c.state.Expires = r.Expires
	c.state.LastSuccess = time.Now()
	c.state.LastError = ""
	c.renewAt = time.Now().Add(time.Until(r.Expires) / 2)
	c.retryAt = time.Time{}
	c.failures = 0
	if !c.held {
		c.state.HeldSince = time.Now()
	}
	c.held = true
	return nil
}

func (m *Manager) reconcile(force, health bool) error {
	if !m.needed.Load() || m.ctx.Err() != nil {
		for _, c := range m.chosen {
			m.cfg.Client.DropReservation(c.state.Peer)
			c.held = false
			c.state.Expires = time.Time{}
			c.state.HeldSince = time.Time{}
		}
		m.chosen = nil
		m.publish()
		return m.ctx.Err()
	}
	var errs []error
	live := m.chosen[:0]
	for _, c := range m.chosen {
		if c.conn.Context().Err() != nil || !c.state.Expires.After(time.Now()) {
			m.failed(c, net.ErrClosed)
			errs = append(errs, net.ErrClosed)
		} else if health {
			ctx, cancel := context.WithTimeout(m.ctx, m.cfg.Timeout)
			rtt, err := m.cfg.Ping(ctx, c.state.Peer)
			cancel()
			if err != nil {
				m.failed(c, err)
				m.cfg.Host.Disconnect(c.state.Peer)
				errs = append(errs, err)
			} else {
				c.state.RTT = rtt
			}
		}
		if c.held && (force || !time.Now().Before(c.renewAt)) {
			if err := m.reserve(c); err != nil {
				errs = append(errs, err)
			}
		}
		if c.held {
			live = append(live, c)
		}
	}
	m.chosen = live
	m.publish() // withdraw before potentially slow replacement probes
	if len(m.chosen) >= 2 {
		return errors.Join(errs...)
	}
	// Measure eligible replacements concurrently with a hard four-probe bound.
	// No worker outlives reconcile; cancel/Close always joins all setup workers.
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, c := range m.candidates {
		if c.held || time.Now().Before(c.retryAt) {
			continue
		}
		wg.Add(1)
		go func(c *candidate) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-m.ctx.Done():
				return
			}
			defer func() { <-slots }()
			ctx, cancel := context.WithTimeout(m.ctx, m.cfg.Timeout)
			defer cancel()
			conn, err := m.cfg.Host.Dial(ctx, c.state.Address)
			if err != nil {
				m.failed(c, err)
				return
			}
			rtt, err := m.cfg.Ping(ctx, c.state.Peer)
			if err != nil {
				m.failed(c, err)
				return
			}
			c.conn = conn
			c.state.RTT = rtt
			c.retryAt = time.Time{}
		}(c)
	}
	wg.Wait()
	for _, c := range m.candidates {
		if c.conn == nil && c.state.LastError != "" {
			errs = append(errs, errors.New(c.state.Peer.String()+": "+c.state.LastError))
		}
	}
	eligible := append([]*candidate(nil), m.candidates...)
	sort.SliceStable(eligible, func(i, j int) bool { return eligible[i].state.RTT < eligible[j].state.RTT })
	for _, c := range eligible {
		if len(m.chosen) >= 2 || m.ctx.Err() != nil || !m.needed.Load() {
			break
		}
		if c.held || c.conn == nil || time.Now().Before(c.retryAt) {
			continue
		}
		if err := m.reserve(c); err != nil {
			errs = append(errs, err)
			continue
		}
		m.chosen = append(m.chosen, c)
	}
	m.publish()
	return errors.Join(errs...)
}
