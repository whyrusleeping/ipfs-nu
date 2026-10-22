package magicsock

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"

	"github.com/whyrusleeping/nu/p2p/wire"
)

var ErrLegExists = errors.New("magicsock: detach the existing packet leg before attaching another")

// PacketLeg is an unreliable packet path to the SAME relayed peer, negotiated by
// the owner (the Nu datagram relay extension, D11c §5). WritePacket reports
// sent=false without error when the current outer budget cannot carry the packet;
// the Relay then writes that packet on the circuit stream. Any error, a failed
// read or Done ends the leg and the Relay falls back to the stream for all
// packets. The stream leg is never replaced: it stays the correctness fallback.
type PacketLeg interface {
	WritePacket(b []byte) (sent bool, err error)
	ReadPacket(ctx context.Context) ([]byte, error)
	Close() error
	Done() <-chan struct{}
}

// ReadFrame decodes /nu/qpath/1: two-byte unsigned big-endian datagram length,
// then exactly 1..1200 bytes of QUIC (fixed bit set). No padding/control frames,
// stream resumption, fragmentation, or non-QUIC probes are carried here.
func ReadFrame(r io.Reader) ([]byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(h[:]))
	if n < 1 || n > PacketSize {
		return nil, wire.ErrLimit
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	if b[0]&0x40 == 0 {
		return nil, wire.ErrFormat
	}
	return b, nil
}
func WriteFrame(w io.Writer, b []byte) error {
	if len(b) < 1 || len(b) > PacketSize {
		return wire.ErrLimit
	}
	if b[0]&0x40 == 0 {
		return wire.ErrFormat
	}
	var h [2]byte
	binary.BigEndian.PutUint16(h[:], uint16(len(b)))
	if err := writeAll(w, h[:]); err != nil {
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

// Relay owns one negotiated circuit. Close cancels and joins both workers, but
// never closes the logical PacketConn or the inner QUIC connection.
type Relay struct {
	c      *Conn
	peer   *Peer
	stream io.ReadWriteCloser
	queue  chan []byte
	done   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
	legMu  sync.Mutex
	leg    PacketLeg
	legEnd context.CancelFunc
	legWG  sync.WaitGroup
}

func (r *Relay) Done() <-chan struct{} { return r.done }
func (r *Relay) stop() {
	r.once.Do(func() {
		close(r.done)
		if abort, ok := r.stream.(interface{ Reset() }); ok {
			abort.Reset()
		} else {
			_ = r.stream.Close()
		}
		r.DetachPacketLeg()
	})
}

// AttachPacketLeg adds a negotiated datagram leg to a live relay. One leg at a
// time; it is closed and detached on its first error, when the Relay closes, or
// by DetachPacketLeg. Ownership of leg transfers on success only.
func (r *Relay) AttachPacketLeg(leg PacketLeg) error {
	if leg == nil {
		return ErrNoPath
	}
	r.legMu.Lock()
	defer r.legMu.Unlock()
	if !relayAlive(r) {
		return ErrNoPath
	}
	if r.leg != nil {
		return ErrLegExists
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.leg, r.legEnd = leg, cancel
	r.legWG.Add(1)
	r.c.wg.Add(1)
	go func() {
		defer r.c.wg.Done()
		defer r.legWG.Done()
		defer r.detach(leg)
		for {
			b, err := leg.ReadPacket(ctx)
			if err != nil {
				return
			}
			if len(b) < 1 || len(b) > PacketSize || b[0]&0x40 == 0 {
				continue
			}
			r.c.datagramReceived.Add(1)
			r.c.deliver(datagram{b: b, addr: net.UDPAddrFromAddrPort(r.peer.addr)})
		}
	}()
	return nil
}
func (r *Relay) detach(leg PacketLeg) {
	r.legMu.Lock()
	if r.leg == leg {
		r.leg = nil
		r.legEnd()
	}
	r.legMu.Unlock()
	_ = leg.Close()
}

// DetachPacketLeg closes/joins the current packet leg; packets use the stream.
func (r *Relay) DetachPacketLeg() {
	r.legMu.Lock()
	leg := r.leg
	r.legMu.Unlock()
	if leg != nil {
		r.detach(leg)
	}
	r.legWG.Wait()
}
func (r *Relay) packetLeg() PacketLeg {
	r.legMu.Lock()
	defer r.legMu.Unlock()
	return r.leg
}

// HasPacketLeg reports whether a datagram leg currently carries packets.
func (r *Relay) HasPacketLeg() bool { return r.packetLeg() != nil }
func (r *Relay) Close() error {
	r.stop()
	r.wg.Wait()
	r.legWG.Wait()
	// Keep the slot occupied even after failure until its workers are joined.
	// Otherwise rapid failing attachments can exceed the two-workers/peer bound.
	r.c.mu.Lock()
	if r.peer.relay == r {
		r.peer.relay = nil
	}
	r.c.mu.Unlock()
	return nil
}
func relayAlive(r *Relay) bool {
	if r == nil {
		return false
	}
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

// AttachRelay takes ownership only on success. stream must already have selected
// /nu/qpath/1 and Close (or Reset) must interrupt concurrent I/O. Existing legs,
// including failed but unjoined legs, are never overwritten. Explicitly close a
// previous handle before reattachment; 0020 owns reconnection/admission policy.
func (c *Conn) AttachRelay(id wire.PeerID, stream io.ReadWriteCloser) (*Relay, error) {
	if stream == nil {
		return nil, ErrNoPath
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, net.ErrClosed
	}
	p := c.peers[id]
	if p == nil {
		c.mu.Unlock()
		return nil, ErrPeer
	}
	if p.relay != nil {
		c.mu.Unlock()
		return nil, ErrRelayExists
	}
	r := &Relay{c: c, peer: p, stream: stream, queue: make(chan []byte, c.relayQueue), done: make(chan struct{})}
	p.relay = r
	r.wg.Add(2)
	c.wg.Add(2)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer r.wg.Done()
		defer r.stop()
		for {
			b, err := ReadFrame(stream)
			if err != nil {
				return
			}
			c.deliver(datagram{b: b, addr: net.UDPAddrFromAddrPort(p.addr)})
		}
	}()
	go func() {
		defer c.wg.Done()
		defer r.wg.Done()
		defer r.stop()
		for {
			select {
			case <-r.done:
				return
			case b := <-r.queue:
				if leg := r.packetLeg(); leg != nil {
					sent, err := leg.WritePacket(b)
					if err != nil {
						r.detach(leg)
					} else if sent {
						c.datagramSent.Add(1)
						continue
					}
				}
				if WriteFrame(stream, b) != nil {
					return
				}
			}
		}
	}()
	return r, nil
}
