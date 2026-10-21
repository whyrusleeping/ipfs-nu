package circuit

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Circuit is a raw byte stream, ready for security-protocol multistream select.
// Peer is asserted by Relay, NOT authenticated end-to-end. Do not hand it to
// content handlers: Nu must negotiate qpath and authenticate inner QUIC first.
// Close/Reset is required even after EOF. Context on Connect bounds setup only.
type Circuit struct {
	*host.Stream
	Relay, Peer wire.PeerID
	Limit       Limit
	done        chan struct{}
	once        sync.Once
}

func (c *Circuit) Close() error {
	err := c.Stream.Close()
	c.once.Do(func() { close(c.done) })
	return err
}
func (c *Circuit) Reset() { c.Stream.Reset(); c.once.Do(func() { close(c.done) }) }

type ClientConfig struct {
	Timeout                      time.Duration
	MaxReservations, MaxCircuits int
}
type heldReservation struct {
	value Reservation
	conn  *host.Conn
}
type Client struct {
	host         *host.Host
	cfg          ClientConfig
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	reservations map[wire.PeerID]heldReservation
	active       map[*Circuit]bool
	accepted     chan *Circuit
	requests     chan struct{}
	wg           sync.WaitGroup
	closeOnce    sync.Once
}

func NewClient(h *host.Host, cfg ClientConfig) (*Client, error) {
	if h == nil || h.Context().Err() != nil {
		return nil, net.ErrClosed
	}
	if cfg.Timeout < 0 || cfg.MaxReservations < 0 || cfg.MaxCircuits < 0 {
		return nil, wire.ErrLimit
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.MaxReservations == 0 {
		cfg.MaxReservations = 8
	}
	if cfg.MaxCircuits == 0 {
		cfg.MaxCircuits = 32
	}
	for _, p := range h.Protocols() {
		if p == StopProtocol {
			return nil, errors.New("circuit: STOP already registered")
		}
	}
	ctx, cancel := context.WithCancel(h.Context())
	c := &Client{host: h, cfg: cfg, ctx: ctx, cancel: cancel, reservations: make(map[wire.PeerID]heldReservation), active: make(map[*Circuit]bool), accepted: make(chan *Circuit, cfg.MaxCircuits), requests: make(chan struct{}, cfg.MaxReservations)}
	if err := h.Handle(StopProtocol, cfg.MaxCircuits+8, c.handleStop); err != nil {
		cancel()
		return nil, err
	}
	return c, nil
}
func (c *Client) begin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil {
		return false
	}
	c.wg.Add(1)
	return true
}
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.cancel()
		for stream := range c.active {
			stream.Reset()
		}
		c.mu.Unlock()
		c.host.RemoveHandler(StopProtocol)
		c.wg.Wait()
	})
	return nil
}
func (c *Client) open(ctx context.Context, relay wire.PeerID) (*host.Stream, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	stopService := context.AfterFunc(c.ctx, cancel)
	s, err := c.host.OpenStreamLazy(ctx, relay, HopProtocol)
	if err != nil {
		stopService()
		cancel()
		return nil, nil, err
	}
	deadline, _ := ctx.Deadline()
	_ = s.SetDeadline(deadline)
	stopIO := context.AfterFunc(ctx, s.Reset)
	var once sync.Once
	return s, func() { once.Do(func() { stopIO(); stopService(); cancel() }) }, nil
}

// Reserve requires an existing direct connection to relay. Vouchers are checked
// when supplied (advisory/optional in the spec); Nu servers always supply them.
func (c *Client) Reserve(ctx context.Context, relay wire.PeerID) (Reservation, error) {
	if !c.begin() {
		return Reservation{}, net.ErrClosed
	}
	defer c.wg.Done()
	select {
	case c.requests <- struct{}{}:
		defer func() { <-c.requests }()
	default:
		return Reservation{}, ResourceLimit
	}
	c.mu.Lock()
	c.pruneLocked()
	_, known := c.reservations[relay]
	full := len(c.reservations) >= c.cfg.MaxReservations
	c.mu.Unlock()
	if !known && full {
		return Reservation{}, ResourceLimit
	}
	s, done, err := c.open(ctx, relay)
	if err != nil {
		return Reservation{}, err
	}
	defer done()
	defer s.Close()
	if err = writeMessage(s, message{kind: 0}, false); err != nil {
		s.Reset()
		return Reservation{}, err
	}
	m, err := readMessage(s, false)
	if err != nil {
		s.Reset()
		return Reservation{}, err
	}
	if m.kind != 2 {
		return Reservation{}, UnexpectedMessage
	}
	if m.status != OK {
		return Reservation{}, m.status
	}
	if m.reservation == nil || !m.reservation.Expires.After(time.Now()) {
		return Reservation{}, MalformedMessage
	}
	r := *m.reservation
	r.Limit = m.limit
	if len(r.Voucher) > 0 {
		if err = VerifyVoucher(r.Voucher, relay, c.host.ID(), r.Expires); err != nil {
			return Reservation{}, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	if c.ctx.Err() != nil {
		return Reservation{}, net.ErrClosed
	}
	if _, ok := c.reservations[relay]; !ok && len(c.reservations) >= c.cfg.MaxReservations {
		return Reservation{}, ResourceLimit
	}
	c.reservations[relay] = heldReservation{value: r, conn: s.Conn()}
	return r, nil
}
func (c *Client) pruneLocked() {
	for id, r := range c.reservations {
		if !r.value.Expires.After(time.Now()) || r.conn.Context().Err() != nil {
			delete(c.reservations, id)
		}
	}
}

// Maintain reserves now and renews halfway to expiration, until cancellation or
// the first failure. The owner selects/redials/replaces relays and republishes
// addresses from update. The callback must return promptly; it may be nil.
func (c *Client) Maintain(ctx context.Context, relay wire.PeerID, update func(Reservation)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	for {
		r, err := c.Reserve(ctx, relay)
		if err != nil {
			return err
		}
		if update != nil {
			update(r)
		}
		timer := time.NewTimer(time.Until(r.Expires) / 2)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}
func (c *Client) Connect(ctx context.Context, relay, peer wire.PeerID) (*Circuit, error) {
	if !c.begin() {
		return nil, net.ErrClosed
	}
	defer c.wg.Done()
	if _, err := wire.PeerIDFromBytes(peer.Bytes()); err != nil {
		return nil, err
	}
	s, done, err := c.open(ctx, relay)
	if err != nil {
		return nil, err
	}
	defer done()
	good := false
	defer func() {
		if !good {
			s.Reset()
		}
	}()
	if err = writeMessage(s, message{kind: 1, peer: peer}, false); err != nil {
		return nil, err
	}
	m, err := readMessage(s, false)
	if err != nil {
		return nil, err
	}
	if m.kind != 2 {
		return nil, UnexpectedMessage
	}
	if m.status != OK {
		return nil, m.status
	}
	circ := &Circuit{Stream: s, Relay: relay, Peer: peer, Limit: m.limit, done: make(chan struct{})}
	c.mu.Lock()
	if c.ctx.Err() != nil || len(c.active) >= c.cfg.MaxCircuits {
		c.mu.Unlock()
		return nil, ResourceLimit
	}
	c.active[circ] = true
	c.wg.Add(1)
	c.mu.Unlock()
	// Detach caller setup cancellation before exposing the circuit. Service and
	// underlying connection lifetime still close it.
	done()
	_ = s.SetDeadline(time.Time{})
	go c.watchCircuit(circ)
	good = true
	return circ, nil
}
func (c *Client) watchCircuit(circ *Circuit) {
	defer c.wg.Done()
	select {
	case <-circ.done:
	case <-c.ctx.Done():
		circ.Reset()
	case <-circ.Conn().Context().Done():
		circ.Reset()
	}
	c.mu.Lock()
	delete(c.active, circ)
	c.mu.Unlock()
}
func (c *Client) Accept(ctx context.Context) (*Circuit, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.ctx.Done():
			return nil, net.ErrClosed
		case circ := <-c.accepted:
			select {
			case <-circ.done:
				continue
			default:
				return circ, nil
			}
		}
	}
}
func (c *Client) handleStop(s *host.Stream) {
	if !c.begin() {
		s.Reset()
		return
	}
	defer c.wg.Done()
	stop := context.AfterFunc(c.ctx, s.Reset)
	defer stop()
	_ = s.SetDeadline(time.Now().Add(c.cfg.Timeout))
	m, err := readMessage(s, true)
	if err != nil {
		_ = writeMessage(s, message{kind: 1, status: MalformedMessage}, true)
		return
	}
	if m.kind != 0 || m.peer == "" {
		_ = writeMessage(s, message{kind: 1, status: UnexpectedMessage}, true)
		return
	}
	c.mu.Lock()
	c.pruneLocked()
	reservation, ok := c.reservations[s.Conn().PeerID()]
	status := OK
	if !ok || reservation.conn != s.Conn() {
		status = PermissionDenied
	} else if len(c.active) >= c.cfg.MaxCircuits {
		status = ResourceLimit
	}
	if status != OK {
		c.mu.Unlock()
		_ = writeMessage(s, message{kind: 1, status: status}, true)
		return
	}
	circ := &Circuit{Stream: s, Relay: s.Conn().PeerID(), Peer: m.peer, Limit: m.limit, done: make(chan struct{})}
	c.active[circ] = true
	c.mu.Unlock()
	defer func() { circ.Close(); c.mu.Lock(); delete(c.active, circ); c.mu.Unlock() }()
	if err = writeMessage(s, message{kind: 1, status: OK}, true); err != nil {
		s.Reset()
		return
	}
	_ = s.SetDeadline(time.Time{})
	select {
	case c.accepted <- circ:
	case <-c.ctx.Done():
		s.Reset()
		return
	}
	select {
	case <-circ.done:
	case <-c.ctx.Done():
		s.Reset()
	case <-s.Conn().Context().Done():
		s.Reset()
	}
}
