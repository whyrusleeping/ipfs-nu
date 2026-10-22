package host

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"
)

// QUIC DATAGRAM (RFC 9221) channels on an authenticated outer connection. Every
// datagram payload is `uint16 big-endian channel | body`. Channel 0 is reserved
// for the fixed-size budget probe/echo; other channels are allocated by the
// protocol that uses them (the Nu datagram relay extension). Datagrams are
// unreliable, unordered and never carry application streams.
const (
	DatagramHeader   = 2
	ProbeChannel     = 0
	maxChannels      = 64
	probeNonceSize   = 8
	probeMinSize     = DatagramHeader + 1 + probeNonceSize
	echoRate         = 32 // echoes per second per connection (burst 64)
	echoBurst        = 64
	datagramQueueMax = 1024
)

var ErrDatagramTooLarge = errors.New("host: datagram exceeds the connection's current payload budget")
var ErrNoDatagrams = errors.New("host: peer connection does not support QUIC datagrams")
var ErrChannelInUse = errors.New("host: datagram channel already registered")

// DatagramChannel receives bodies for one registered channel ID. Full queues
// drop (datagrams are lossy by definition); Close unregisters the channel.
type DatagramChannel struct {
	conn  *Conn
	id    uint16
	queue chan []byte
	done  chan struct{}
	once  sync.Once
}

func (ch *DatagramChannel) ID() uint16 { return ch.id }
func (ch *DatagramChannel) Receive(ctx context.Context) ([]byte, error) {
	select {
	case b := <-ch.queue:
		return b, nil
	default:
	}
	select {
	case b := <-ch.queue:
		return b, nil
	case <-ch.done:
		return nil, net.ErrClosed
	case <-ch.conn.Context().Done():
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (ch *DatagramChannel) Close() error {
	ch.once.Do(func() {
		c := ch.conn
		c.dmu.Lock()
		if c.channels[ch.id] == ch {
			delete(c.channels, ch.id)
		}
		c.dmu.Unlock()
		close(ch.done)
	})
	return nil
}

type datagramState struct {
	dmu       sync.Mutex
	channels  map[uint16]*DatagramChannel
	probes    map[[probeNonceSize]byte]chan struct{}
	echoes    float64
	echoAt    time.Time
	dropped   uint64
	readStart bool
}

// SupportsDatagrams is true when both ends negotiated RFC 9221 on this QUIC
// connection. WSS/TCP connections never support datagrams.
func (c *Conn) SupportsDatagrams() bool {
	if c.quic == nil {
		return false
	}
	state := c.quic.ConnectionState()
	return state.SupportsDatagrams.Local && state.SupportsDatagrams.Remote
}

// OpenDatagramChannel registers receive interest for a nonzero channel. The ID
// is assigned by the protocol owner (relays allocate per connection); duplicate
// registration fails. queue bounds buffered bodies (default 64).
func (c *Conn) OpenDatagramChannel(id uint16, queue int) (*DatagramChannel, error) {
	if !c.SupportsDatagrams() {
		return nil, ErrNoDatagrams
	}
	if id == ProbeChannel || queue < 0 || queue > datagramQueueMax {
		return nil, ErrLimit
	}
	if queue == 0 {
		queue = 64
	}
	c.dmu.Lock()
	defer c.dmu.Unlock()
	if c.Context().Err() != nil {
		return nil, net.ErrClosed
	}
	if c.channels == nil {
		c.channels = make(map[uint16]*DatagramChannel)
	}
	if c.channels[id] != nil {
		return nil, ErrChannelInUse
	}
	if len(c.channels) >= maxChannels {
		return nil, ErrLimit
	}
	ch := &DatagramChannel{conn: c, id: id, queue: make(chan []byte, queue), done: make(chan struct{})}
	c.channels[id] = ch
	c.startDatagramReader()
	return ch, nil
}

// SendDatagram queues one unreliable datagram. ErrDatagramTooLarge reports the
// current budget (outer packet size minus headers); callers must fall back to a
// reliable path for that packet rather than fragment it.
func (c *Conn) SendDatagram(channel uint16, body []byte) error {
	if !c.SupportsDatagrams() {
		return ErrNoDatagrams
	}
	if len(body) == 0 || len(body) > 65535-DatagramHeader {
		return ErrLimit
	}
	payload := make([]byte, DatagramHeader+len(body))
	binary.BigEndian.PutUint16(payload, channel)
	copy(payload[DatagramHeader:], body)
	err := c.quic.SendDatagram(payload)
	var tooLarge *quic.DatagramTooLargeError
	if errors.As(err, &tooLarge) {
		return ErrDatagramTooLarge
	}
	return err
}

// ProbeDatagram sends a channel-0 probe whose complete payload is exactly size
// bytes and waits for the peer's same-size echo. Success proves that this
// connection currently carries size-byte datagrams in both directions.
func (c *Conn) ProbeDatagram(ctx context.Context, size int) (time.Duration, error) {
	if !c.SupportsDatagrams() {
		return 0, ErrNoDatagrams
	}
	if size < probeMinSize || size > 65535 {
		return 0, ErrLimit
	}
	payload := make([]byte, size)
	payload[DatagramHeader] = 1
	var nonce [probeNonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return 0, err
	}
	copy(payload[DatagramHeader+1:], nonce[:])
	echoed := make(chan struct{}, 1)
	c.dmu.Lock()
	if c.probes == nil {
		c.probes = make(map[[probeNonceSize]byte]chan struct{})
	}
	if len(c.probes) >= 16 {
		c.dmu.Unlock()
		return 0, ErrLimit
	}
	c.probes[nonce] = echoed
	c.startDatagramReader()
	c.dmu.Unlock()
	defer func() {
		c.dmu.Lock()
		delete(c.probes, nonce)
		c.dmu.Unlock()
	}()
	start := time.Now()
	err := c.quic.SendDatagram(payload)
	var tooLarge *quic.DatagramTooLargeError
	if errors.As(err, &tooLarge) {
		return 0, ErrDatagramTooLarge
	}
	if err != nil {
		return 0, err
	}
	select {
	case <-echoed:
		return time.Since(start), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-c.Context().Done():
		return 0, net.ErrClosed
	}
}

// DatagramsDropped counts received datagrams discarded for unknown channels or
// full queues (diagnostics).
func (c *Conn) DatagramsDropped() uint64 {
	c.dmu.Lock()
	defer c.dmu.Unlock()
	return c.dropped
}

// startDatagramReader is called with dmu held. One reader per connection
// dispatches by channel; it exits with the connection (quic-go's own bounded
// receive queue absorbs datagrams before the first registration).
func (c *Conn) startDatagramReader() {
	if c.readStart || c.quic == nil {
		return
	}
	c.readStart = true
	go func() {
		for {
			payload, err := c.quic.ReceiveDatagram(c.Context())
			if err != nil {
				return
			}
			c.dispatchDatagram(payload)
		}
	}()
}
func (c *Conn) dispatchDatagram(payload []byte) {
	if len(payload) < DatagramHeader+1 {
		return
	}
	id := binary.BigEndian.Uint16(payload)
	if id == ProbeChannel {
		c.handleProbe(payload)
		return
	}
	c.dmu.Lock()
	ch := c.channels[id]
	c.dmu.Unlock()
	if ch == nil {
		c.dmu.Lock()
		c.dropped++
		c.dmu.Unlock()
		return
	}
	select {
	case ch.queue <- payload[DatagramHeader:]:
	default:
		c.dmu.Lock()
		c.dropped++
		c.dmu.Unlock()
	}
}
func (c *Conn) handleProbe(payload []byte) {
	if len(payload) < probeMinSize {
		return
	}
	switch payload[DatagramHeader] {
	case 1:
		// Echo at most echoRate/s back to the authenticated sender on the same
		// connection, with the same size: a reflector, never an amplifier.
		c.dmu.Lock()
		now := time.Now()
		if c.echoAt.IsZero() {
			c.echoes = echoBurst
		} else {
			c.echoes = min(echoBurst, c.echoes+now.Sub(c.echoAt).Seconds()*echoRate)
		}
		c.echoAt = now
		allowed := c.echoes >= 1
		if allowed {
			c.echoes--
		}
		c.dmu.Unlock()
		if !allowed {
			return
		}
		echo := append([]byte(nil), payload...)
		echo[DatagramHeader] = 2
		_ = c.quic.SendDatagram(echo)
	case 2:
		var nonce [probeNonceSize]byte
		copy(nonce[:], payload[DatagramHeader+1:])
		c.dmu.Lock()
		ch := c.probes[nonce]
		c.dmu.Unlock()
		if ch != nil {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}
