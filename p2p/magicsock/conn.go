package magicsock

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
	"golang.org/x/net/ipv4"
)

const PacketSize = 1200

// ConnectionIDLength must also be set on quic.Transport (Host does this).
const ConnectionIDLength = 8

var ErrLimit = errors.New("magicsock: resource limit")
var ErrNoPath = errors.New("magicsock: no selected path")
var ErrPeer = errors.New("magicsock: unknown or mismatched peer")
var ErrRelayExists = errors.New("magicsock: detach existing relay before attaching another")

// Config owns one UDP socket. New takes ownership on success only. Zero limits
// select defaults. ReceiveQueue bounds relay injection; direct UDP is received
// into the QUIC caller's buffers, without a second userspace queue. A path set
// is authorization, not an untrusted address hint.
type Config struct {
	UDP                                *net.UDPConn
	MaxPeers, ReceiveQueue, RelayQueue int
}
type Peer struct {
	id       wire.PeerID
	addr     netip.AddrPort
	direct   map[netip.AddrPort]bool
	selected netip.AddrPort
	relay    *Relay
}
type cidRoute struct {
	peer  *Peer
	owner *cidTrace
}
type datagram struct {
	b, oob []byte
	addr   net.Addr
	flags  int
}

// Conn multiplexes normal QUIC, inner QUIC and non-QUIC probes on one UDP socket.
// Unknown CIDs and physical addresses pass through unchanged. It must be the sole
// reader of UDP; give Conn, not the underlying socket, to quic-go.
type Conn struct {
	udp                         *net.UDPConn
	packetUDP                   *ipv4.PacketConn
	readMu                      sync.Mutex // serializes physical reads and caller-owned batch buffers
	readingUDP                  bool       // protected by mu; relay enqueue wakes an outstanding fd read
	maxPeers, relayQueue        int
	mu                          sync.Mutex
	peers                       map[wire.PeerID]*Peer
	virtual                     map[netip.AddrPort]*Peer
	cids                        map[cidKey]cidRoute
	next                        uint32
	closed                      bool
	incoming                    chan datagram
	disco                       chan datagram
	done                        chan struct{}
	readDone                    chan struct{}
	readErr                     error
	readDeadline, writeDeadline time.Time
	wg                          sync.WaitGroup
	closeOnce                   sync.Once
	dropped                     atomic.Uint64
	received                    atomic.Uint64
	relayed                     atomic.Uint64
	directSent                  atomic.Uint64
	datagramSent                atomic.Uint64
	datagramReceived            atomic.Uint64
}

func New(cfg Config) (*Conn, error) {
	if cfg.UDP == nil || cfg.MaxPeers < 0 || cfg.ReceiveQueue < 0 || cfg.RelayQueue < 0 {
		return nil, ErrLimit
	}
	if cfg.MaxPeers == 0 {
		cfg.MaxPeers = 128
	}
	if cfg.ReceiveQueue == 0 {
		cfg.ReceiveQueue = 256
	}
	if cfg.RelayQueue == 0 {
		cfg.RelayQueue = 64
	}
	if cfg.MaxPeers > 65536 || cfg.ReceiveQueue > 65536 || cfg.RelayQueue > 4096 {
		return nil, ErrLimit
	}
	c := &Conn{udp: cfg.UDP, packetUDP: ipv4.NewPacketConn(cfg.UDP), maxPeers: cfg.MaxPeers, relayQueue: cfg.RelayQueue, peers: make(map[wire.PeerID]*Peer), virtual: make(map[netip.AddrPort]*Peer), cids: make(map[cidKey]cidRoute), incoming: make(chan datagram, cfg.ReceiveQueue), disco: make(chan datagram, 32), done: make(chan struct{}), readDone: make(chan struct{})}
	return c, nil
}

// AddPeer returns a stable, local-only virtual address. Never publish or persist
// it as a peer candidate. Removed addresses are never reused by this Conn.
func (c *Conn) AddPeer(id wire.PeerID) (netip.AddrPort, error) {
	if _, err := wire.PeerIDFromBytes(id.Bytes()); err != nil {
		return netip.AddrPort{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return netip.AddrPort{}, net.ErrClosed
	}
	if p := c.peers[id]; p != nil {
		return p.addr, nil
	}
	if len(c.peers) >= c.maxPeers || c.next >= 0x0ffffffe {
		return netip.AddrPort{}, ErrLimit
	}
	c.next++
	ip := netip.AddrFrom4([4]byte{240 | byte(c.next>>24), byte(c.next >> 16), byte(c.next >> 8), byte(c.next)})
	if c.udp.LocalAddr().(*net.UDPAddr).IP.To4() == nil {
		var b [16]byte
		b[0] = 1 // RFC 6666 discard-only 100::/64, never a physical candidate.
		b[12] = byte(c.next >> 24)
		b[13] = byte(c.next >> 16)
		b[14] = byte(c.next >> 8)
		b[15] = byte(c.next)
		ip = netip.AddrFrom16(b)
	}
	addr := netip.AddrPortFrom(ip, 1)
	p := &Peer{id: id, addr: addr, direct: make(map[netip.AddrPort]bool)}
	c.peers[id] = p
	c.virtual[addr] = p
	return addr, nil
}
func (c *Conn) PeerAddr(id wire.PeerID) (netip.AddrPort, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.peers[id]
	if p == nil {
		return netip.AddrPort{}, false
	}
	return p.addr, true
}
func (c *Conn) RemovePeer(id wire.PeerID) {
	c.mu.Lock()
	p := c.peers[id]
	var relay *Relay
	if p != nil {
		relay = p.relay
		delete(c.peers, id)
		delete(c.virtual, p.addr)
		for k, r := range c.cids {
			if r.peer == p {
				delete(c.cids, k)
			}
		}
	}
	c.mu.Unlock()
	if relay != nil {
		relay.Close()
	}
}

// SetDirectPaths replaces the authorized/validated endpoints (max 32). It does
// not probe, authorize destinations or select a path. 0017 owns those decisions.
func (c *Conn) SetDirectPaths(id wire.PeerID, paths []netip.AddrPort) error {
	if len(paths) > 32 {
		return ErrLimit
	}
	next := make(map[netip.AddrPort]bool, len(paths))
	for _, a := range paths {
		if !a.IsValid() || a.Port() == 0 || a.Addr().IsUnspecified() || a.Addr().IsMulticast() || IsVirtual(a) {
			return wire.ErrFormat
		}
		a = netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
		next[a] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	p := c.peers[id]
	if p == nil {
		return ErrPeer
	}
	p.direct = next
	if !next[p.selected] {
		p.selected = netip.AddrPort{}
	}
	return nil
}
func (c *Conn) UseDirect(id wire.PeerID, a netip.AddrPort) error {
	a = netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	p := c.peers[id]
	if p == nil {
		return ErrPeer
	}
	if !p.direct[a] {
		return ErrNoPath
	}
	p.selected = a
	return nil
}
func (c *Conn) UseRelay(id wire.PeerID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	p := c.peers[id]
	if p == nil {
		return ErrPeer
	}
	if !relayAlive(p.relay) {
		return ErrNoPath
	}
	p.selected = netip.AddrPort{}
	return nil
}

// PathSet is a snapshot. Direct endpoints have been authorized by the caller;
// Selected is invalid when using the relay (or awaiting a relay). Order is undefined.
type PathSet struct {
	Direct   []netip.AddrPort
	Selected netip.AddrPort
	Relay    bool
	Datagram bool // the relay leg currently has a negotiated datagram packet leg
}

func (c *Conn) Paths(id wire.PeerID) (PathSet, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.peers[id]
	if p == nil || c.closed {
		return PathSet{}, false
	}
	set := PathSet{Selected: p.selected, Relay: relayAlive(p.relay)}
	if set.Relay {
		set.Datagram = p.relay.HasPacketLeg()
	}
	for a := range p.direct {
		set.Direct = append(set.Direct, a)
	}
	return set, true
}

// VerifyPeerAddress is the optional host admission hook. Circuit metadata does
// NOT authenticate the claimed peer: libp2p TLS must match the virtual address.
func (c *Conn) VerifyPeerAddress(a net.Addr, id wire.PeerID) error {
	addr, ok := addrPort(a)
	if !ok || !IsVirtual(addr) {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.virtual[addr]
	if p == nil || p.id != id {
		return ErrPeer
	}
	return nil
}
func (c *Conn) IsLocalOnlyAddress(addr net.Addr) bool {
	a, ok := addrPort(addr)
	return ok && IsVirtual(a)
}

func IsVirtual(a netip.AddrPort) bool {
	ip := a.Addr().Unmap()
	if ip.Is4() {
		b := ip.As4()
		return b[0]&0xf0 == 0xf0
	}
	if ip.Is6() {
		b := ip.As16()
		return b[0] == 1 && b[1] == 0 && b[2] == 0 && b[3] == 0 && b[4] == 0 && b[5] == 0 && b[6] == 0 && b[7] == 0
	}
	return false
}
func addrPort(a net.Addr) (netip.AddrPort, bool) {
	u, ok := a.(*net.UDPAddr)
	if !ok {
		return netip.AddrPort{}, false
	}
	v := u.AddrPort()
	return netip.AddrPortFrom(v.Addr().Unmap(), v.Port()), v.IsValid()
}
func (c *Conn) LocalAddr() net.Addr                   { return c.udp.LocalAddr() }
func (c *Conn) SyscallConn() (syscall.RawConn, error) { return c.udp.SyscallConn() }
func (c *Conn) SetReadBuffer(n int) error             { return c.udp.SetReadBuffer(n) }
func (c *Conn) SetWriteBuffer(n int) error            { return c.udp.SetWriteBuffer(n) }
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	c.readDeadline = t
	if c.readingUDP && len(c.incoming) > 0 {
		t = time.Now()
	}
	return c.udp.SetReadDeadline(t)
}
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	c.writeDeadline = t
	return c.udp.SetWriteDeadline(t)
}
func (c *Conn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		close(c.done)
		var relays []*Relay
		for _, p := range c.peers {
			if p.relay != nil {
				relays = append(relays, p.relay)
			}
		}
		c.mu.Unlock()
		_ = c.udp.Close()
		for _, r := range relays {
			r.Close()
		}
		c.wg.Wait()
		c.readMu.Lock()
		drainPackets(c.incoming)
		drainPackets(c.disco)
		c.readMu.Unlock()
		c.mu.Lock()
		clear(c.peers)
		clear(c.virtual)
		clear(c.cids)
		c.mu.Unlock()
	})
	return nil
}
func drainPackets(ch chan datagram) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// Stats: Relayed counts packets queued for the relay leg (stream or datagram);
// DatagramSent/DatagramReceived count those carried by a negotiated packet leg.
type Stats struct {
	Received, Relayed, DirectSent, Dropped uint64
	DatagramSent, DatagramReceived         uint64
	Peers, CIDs                            int
}

func (c *Conn) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{c.received.Load(), c.relayed.Load(), c.directSent.Load(), c.dropped.Load(), c.datagramSent.Load(), c.datagramReceived.Load(), len(c.peers), len(c.cids)}
}
func (c *Conn) deliver(d datagram) {
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.incoming <- d:
		c.received.Add(1)
		c.mu.Lock()
		if c.readingUDP {
			_ = c.udp.SetReadDeadline(time.Now())
		}
		c.mu.Unlock()
	default:
		c.dropped.Add(1)
	}
}
