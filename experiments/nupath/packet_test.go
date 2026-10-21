// Package nupath is a disposable stock-quic-go feasibility harness, not a transport.
package nupath

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

const frameMax = 1500 // Disposable framing, NOT a published /nu/qpath/1 wire format.
var virtual = &net.UDPAddr{IP: net.IPv4(100, 64, 0, 1), Port: 4242}

type packet struct {
	b    []byte
	addr net.Addr
	due  time.Time
}
type relayPath struct {
	io.ReadWriteCloser
	queue chan []byte
	done  chan struct{}
	once  sync.Once
}

func (r *relayPath) close() { r.once.Do(func() { close(r.done); _ = r.Close() }) }

type packetConn struct {
	udp           *net.UDPConn
	peer          *net.UDPAddr
	incoming      chan packet
	outgoing      chan packet
	done          chan struct{}
	once          sync.Once
	wg            sync.WaitGroup
	mu            sync.Mutex
	relay         *relayPath
	direct        atomic.Bool
	blackhole     atomic.Bool
	delay         atomic.Int64
	lossEvery     atomic.Int64
	mtu           atomic.Int64
	sent          atomic.Int64
	dropped       atomic.Int64
	oversize      atomic.Int64
	directPackets atomic.Int64
	relayPackets  atomic.Int64
	maxSize       atomic.Int64
	maxQueue      atomic.Int64
}

func newPacketConn(bind string) (*packetConn, error) {
	addr, err := net.ResolveUDPAddr("udp4", bind)
	if err != nil {
		return nil, err
	}
	udp, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, err
	}
	return packetConnFromUDP(udp), nil
}

func packetConnFromUDP(udp *net.UDPConn) *packetConn {
	p := &packetConn{udp: udp, incoming: make(chan packet, 256), outgoing: make(chan packet, 256), done: make(chan struct{})}
	p.mtu.Store(frameMax)
	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		for {
			b := make([]byte, frameMax+1)
			n, addr, err := udp.ReadFromUDP(b)
			if err != nil {
				return
			}
			if n > 0 && b[0]&0x40 != 0 {
				addr = virtual
			}
			p.deliver(packet{b: b[:n], addr: addr})
		}
	}()
	go func() {
		defer p.wg.Done()
		for {
			select {
			case <-p.done:
				return
			case pkt := <-p.outgoing:
				if wait := time.Until(pkt.due); wait > 0 {
					timer := time.NewTimer(wait)
					select {
					case <-timer.C:
					case <-p.done:
						timer.Stop()
						return
					}
				}
				_, _ = udp.WriteTo(pkt.b, pkt.addr)
			}
		}
	}()
	return p
}
func (p *packetConn) deliver(pkt packet) {
	select {
	case p.incoming <- pkt:
	case <-p.done:
	default:
		p.dropped.Add(1)
	}
}
func (p *packetConn) attach(stream io.ReadWriteCloser) {
	r := &relayPath{ReadWriteCloser: stream, queue: make(chan []byte, 64), done: make(chan struct{})}
	p.mu.Lock()
	old := p.relay
	p.relay = r
	p.mu.Unlock()
	if old != nil {
		old.close()
	}
	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		defer r.close()
		for {
			b, err := wire.ReadMessage(r, frameMax)
			if err != nil {
				return
			}
			if !p.blackhole.Load() {
				p.deliver(packet{b: b, addr: virtual})
			}
		}
	}()
	go func() {
		defer p.wg.Done()
		defer r.close()
		for {
			select {
			case <-p.done:
				return
			case <-r.done:
				return
			case b := <-r.queue:
				if wire.WriteMessage(r, b, frameMax) != nil {
					return
				}
			}
		}
	}()
}
func (p *packetConn) detach() {
	p.mu.Lock()
	r := p.relay
	p.relay = nil
	p.mu.Unlock()
	if r != nil {
		r.close()
	}
}
func (p *packetConn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case <-p.done:
		return 0, nil, net.ErrClosed
	case pkt := <-p.incoming:
		return copy(b, pkt.b), pkt.addr, nil
	}
}
func (p *packetConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	select {
	case <-p.done:
		return 0, net.ErrClosed
	default:
	}
	// Non-QUIC probes use real endpoints, not the virtual peer route.
	if len(b) > 0 && b[0]&0x40 == 0 {
		return p.udp.WriteTo(b, addr)
	}
	n := p.sent.Add(1)
	p.maxSize.Store(max(p.maxSize.Load(), int64(len(b))))
	if p.blackhole.Load() {
		p.dropped.Add(1)
		return len(b), nil
	}
	if int64(len(b)) > p.mtu.Load() {
		p.oversize.Add(1)
		return len(b), nil
	}
	if loss := p.lossEvery.Load(); loss > 0 && n%loss == 0 {
		p.dropped.Add(1)
		return len(b), nil
	}
	data := append([]byte(nil), b...)
	if p.direct.Load() {
		p.directPackets.Add(1)
		select {
		case p.outgoing <- packet{b: data, addr: p.peer, due: time.Now().Add(time.Duration(p.delay.Load()))}:
			p.maxQueue.Store(max(p.maxQueue.Load(), int64(len(p.outgoing))))
		default:
			p.dropped.Add(1)
		}
	} else {
		p.relayPackets.Add(1)
		p.mu.Lock()
		r := p.relay
		p.mu.Unlock()
		if r != nil {
			select {
			case r.queue <- data:
				p.maxQueue.Store(max(p.maxQueue.Load(), int64(len(r.queue))))
			case <-r.done:
			default:
				p.dropped.Add(1)
			}
		} else {
			p.dropped.Add(1)
		}
	}
	return len(b), nil
}
func (p *packetConn) Close() error {
	p.once.Do(func() { close(p.done); _ = p.udp.Close(); p.detach(); p.wg.Wait() })
	return nil
}
func (p *packetConn) LocalAddr() net.Addr { return p.udp.LocalAddr() }

// The harness only needs an unset read deadline (Transport.Close). Finite I/O
// deadlines are deliberately unsupported rather than silently being ignored.
func (p *packetConn) SetDeadline(t time.Time) error { return p.SetReadDeadline(t) }
func (p *packetConn) SetReadDeadline(t time.Time) error {
	if !t.IsZero() {
		return errors.New("spike: finite PacketConn deadline unsupported")
	}
	return nil
}
func (p *packetConn) SetWriteDeadline(t time.Time) error { return p.SetReadDeadline(t) }
func (p *packetConn) SetReadBuffer(n int) error          { return p.udp.SetReadBuffer(n) }
func (p *packetConn) SetWriteBuffer(n int) error         { return p.udp.SetWriteBuffer(n) }

// Expose DF support without claiming ECN/GSO: lets stock DPLPMTUD run in the
// negative MTU experiment. Never implement net.Conn: x/net may bypass ReadFrom.
func (p *packetConn) SyscallConn() (syscall.RawConn, error) { return p.udp.SyscallConn() }

func waitUntil(ctx context.Context, f func() bool) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if f() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
