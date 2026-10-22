package magicsock

import (
	"encoding/binary"
	"io"
	"net"
	"sync"

	"github.com/whyrusleeping/nu/p2p/wire"
)

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
	})
}
func (r *Relay) Close() error {
	r.stop()
	r.wg.Wait()
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
			c.receivedBytes.relay.Add(uint64(len(b)))
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
				if WriteFrame(stream, b) != nil {
					return
				}
			}
		}
	}()
	return r, nil
}
