package circuit

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// DatagramLeg is one endpoint of a negotiated datagram relay pair. It satisfies
// magicsock.PacketLeg: packets that exceed the current outer budget are reported
// as not sent so the owner writes them on the circuit stream instead. Close ends
// the control stream (the relay then tears down the pair) and frees the channel.
type DatagramLeg struct {
	Relay, Peer wire.PeerID
	conn        *host.Conn
	stream      *host.Stream
	channel     *host.DatagramChannel
	id          uint16
	done        chan struct{}
	once        sync.Once
	sent        atomic.Uint64
	received    atomic.Uint64
	tooLarge    atomic.Uint64
}

func (l *DatagramLeg) Done() <-chan struct{} { return l.done }
func (l *DatagramLeg) Channel() uint16       { return l.id }

// Stats returns datagrams sent, received and refused for budget (then written
// on the stream by the owner).
func (l *DatagramLeg) Stats() (sent, received, tooLarge uint64) {
	return l.sent.Load(), l.received.Load(), l.tooLarge.Load()
}
func (l *DatagramLeg) WritePacket(b []byte) (bool, error) {
	select {
	case <-l.done:
		return false, net.ErrClosed
	default:
	}
	err := l.conn.SendDatagram(l.id, b)
	if errors.Is(err, host.ErrDatagramTooLarge) {
		l.tooLarge.Add(1)
		return false, nil
	}
	if err != nil {
		l.Close()
		return false, err
	}
	l.sent.Add(1)
	return true, nil
}
func (l *DatagramLeg) ReadPacket(ctx context.Context) ([]byte, error) {
	for {
		b, err := l.channel.Receive(ctx)
		if err != nil {
			select {
			case <-l.done:
				return nil, net.ErrClosed
			default:
			}
			if ctx.Err() != nil {
				return nil, err
			}
			l.Close()
			return nil, err
		}
		if !validInnerPacket(b) {
			continue
		}
		l.received.Add(1)
		return b, nil
	}
}
func (l *DatagramLeg) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.stream.Reset()
		l.channel.Close()
	})
	return nil
}
func (l *DatagramLeg) watch(ctx context.Context) {
	go func() {
		drainControl(l.stream)
		l.Close()
	}()
	go func() {
		select {
		case <-l.done:
		case <-ctx.Done():
			l.Close()
		case <-l.conn.Context().Done():
			l.Close()
		}
	}()
}

// UpgradeDatagram asks relay to carry the already-established circuit pair with
// peer as datagrams. It needs the direct relay connection and returns only after
// the relay proved both legs' budgets (READY). Failure leaves the circuit stream
// untouched; callers retry with backoff, not in a loop.
func (c *Client) UpgradeDatagram(ctx context.Context, relay, peer wire.PeerID) (*DatagramLeg, error) {
	if !c.begin() {
		return nil, net.ErrClosed
	}
	defer c.wg.Done()
	if _, err := wire.PeerIDFromBytes(peer.Bytes()); err != nil {
		return nil, err
	}
	conn, ok := c.host.Connection(relay)
	if !ok {
		return nil, host.ErrNoConnection
	}
	if !conn.SupportsDatagrams() {
		return nil, ErrDatagramUnsupported
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	s, err := conn.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	good := false
	defer func() {
		if !good {
			s.Reset()
		}
	}()
	deadline, _ := ctx.Deadline()
	_ = s.SetDeadline(deadline)
	stopIO := context.AfterFunc(ctx, s.Reset)
	defer stopIO()
	if err = wire.Select(s, DatagramHopProtocol); err != nil {
		return nil, errors.Join(ErrDatagramUnsupported, err)
	}
	if err = writeDatagramMessage(s, datagramMessage{kind: dgRequest, peer: peer}); err != nil {
		return nil, err
	}
	m, err := readDatagramMessage(s)
	if err != nil {
		return nil, err
	}
	if m.kind == dgFail {
		return nil, errors.Join(ErrDatagramUnsupported, m.status)
	}
	if m.kind != dgAccept {
		return nil, UnexpectedMessage
	}
	channel, err := conn.OpenDatagramChannel(m.channel, datagramQueue)
	if err != nil {
		return nil, err
	}
	if m, err = readDatagramMessage(s); err != nil || m.kind != dgReady {
		channel.Close()
		if err == nil && m.kind == dgFail {
			return nil, errors.Join(ErrDatagramUnsupported, m.status)
		}
		if err == nil {
			err = UnexpectedMessage
		}
		return nil, err
	}
	leg := &DatagramLeg{Relay: relay, Peer: peer, conn: conn, stream: s, channel: channel, id: channel.ID(), done: make(chan struct{})}
	if !stopIO() {
		leg.Close()
		return nil, ctx.Err()
	}
	_ = s.SetDeadline(time.Time{})
	leg.watch(c.ctx)
	good = true
	return leg, nil
}

// AcceptDatagram delivers legs offered by relays for peers the owner's
// ClientConfig.AcceptDatagram policy approved. The owner attaches each leg to
// its live circuit for that peer or closes it.
func (c *Client) AcceptDatagram(ctx context.Context) (*DatagramLeg, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.ctx.Done():
			return nil, net.ErrClosed
		case leg := <-c.acceptedDatagram:
			select {
			case <-leg.done:
				continue
			default:
				return leg, nil
			}
		}
	}
}
func (c *Client) handleDatagramStop(s *host.Stream) {
	if !c.begin() {
		s.Reset()
		return
	}
	defer c.wg.Done()
	stop := context.AfterFunc(c.ctx, s.Reset)
	defer stop()
	_ = s.SetDeadline(time.Now().Add(c.cfg.Timeout))
	fail := func(status Status) { _ = writeDatagramMessage(s, datagramMessage{kind: dgFail, status: status}) }
	m, err := readDatagramMessage(s)
	if err != nil || m.kind != dgRequest || m.channel == 0 {
		fail(MalformedMessage)
		return
	}
	relay := s.Conn().PeerID()
	c.mu.Lock()
	c.pruneLocked()
	reservation, ok := c.reservations[relay]
	c.mu.Unlock()
	// Only our reserved relay, on the exact reserving connection, may offer; the
	// owner policy confirms a live circuit with that peer exists.
	if !ok || reservation.conn != s.Conn() || c.cfg.AcceptDatagram == nil || !c.cfg.AcceptDatagram(relay, m.peer) {
		fail(PermissionDenied)
		return
	}
	if !s.Conn().SupportsDatagrams() {
		fail(PermissionDenied)
		return
	}
	channel, err := s.Conn().OpenDatagramChannel(m.channel, datagramQueue)
	if err != nil {
		fail(ResourceLimit)
		return
	}
	leg := &DatagramLeg{Relay: relay, Peer: m.peer, conn: s.Conn(), stream: s, channel: channel, id: m.channel, done: make(chan struct{})}
	defer leg.Close()
	if err = writeDatagramMessage(s, datagramMessage{kind: dgAccept, channel: m.channel}); err != nil {
		return
	}
	if m, err = readDatagramMessage(s); err != nil || m.kind != dgReady {
		return
	}
	_ = s.SetDeadline(time.Time{})
	select {
	case c.acceptedDatagram <- leg:
	default:
		return
	}
	go func() {
		select {
		case <-leg.done:
		case <-c.ctx.Done():
			leg.Close()
		case <-s.Conn().Context().Done():
			leg.Close()
		}
	}()
	drainControl(s)
}
