package swarm

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/p2p/homerelay"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/magicsock"
	p2pproto "github.com/whyrusleeping/nu/p2p/proto"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/proto/qpath"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// connectivity is the socket/path owner, deliberately separate from fetch and
// application protocols. Lookup sources (including the future 0072 DHT) supply
// session addresses; they never bypass this owner's relay/destination admission.
// One primary socket carries inner paths; other families still serve ordinary
// wire-compatible direct/outer QUIC. Inner cross-family multipath is not claimed.
type connectivity struct {
	s         *Swarm
	ctx       context.Context
	cancel    context.CancelFunc
	socket    *magicsock.Conn
	client    *circuit.Client
	homes     *homerelay.Manager
	network   *networkControl
	mu        sync.Mutex
	closed    bool
	peers     map[wire.PeerID]*pathPeer
	known     map[wire.PeerID]homerelay.Endpoints
	pending   chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	historyMu sync.Mutex // per-peer path history for doctor; leaf lock (see note)
	history   map[wire.PeerID][]apiv0.PathEvent
}

type pathPeer struct {
	gate          chan struct{}
	leg           *magicsock.Relay // guarded by gate; close/join before every replacement
	routes        []wire.Multiaddr
	seq           uint64
	touched       time.Time
	relay         wire.PeerID // guarded by gate; diagnostics, not identity
	lastError     string
	lastConnected time.Time
	setupTime     time.Duration // last successful raw-circuit + inner TLS setup span
	// 0019 relay optimization, guarded by connectivity.mu (relay_optimizer.go).
	datagram    string // "", "stream", "datagram" or "stream: <last upgrade error>"
	relayChoice string // last pair-aware choice or error
	optimizing  bool
}

// sharedHost binds all sockets before Host starts, preserving its ownership-on-
// error contract and the daemon's ephemeral-port retry. No physical socket is
// replaced after startup: the D12 hook must report that limitation honestly.
func sharedHost(cfg host.Config) (*host.Host, *magicsock.Conn, error) {
	listen := cfg.Listen
	if len(listen) == 0 {
		listen = []string{"127.0.0.1:0"}
	}
	cfg.Listen = nil
	var primary *magicsock.Conn
	for _, addr := range listen {
		udpAddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			for _, p := range cfg.PacketConns {
				p.Close()
			}
			return nil, nil, err
		}
		family := "udp6"
		if udpAddr.IP.To4() != nil {
			family = "udp4"
		}
		udp, err := net.ListenUDP(family, udpAddr)
		if err != nil {
			for _, p := range cfg.PacketConns {
				p.Close()
			}
			return nil, nil, err
		}
		socket, err := magicsock.New(magicsock.Config{UDP: udp})
		if err != nil {
			udp.Close()
			for _, p := range cfg.PacketConns {
				p.Close()
			}
			return nil, nil, err
		}
		cfg.PacketConns = append(cfg.PacketConns, socket)
		if primary == nil || udpAddr.IP.To4() != nil {
			primary = socket
		}
	}
	h, err := host.New(cfg)
	return h, primary, err
}

func (s *Swarm) startConnectivity(ctx context.Context, socket *magicsock.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	c := &connectivity{s: s, ctx: ctx, cancel: cancel, socket: socket, peers: make(map[wire.PeerID]*pathPeer), known: make(map[wire.PeerID]homerelay.Endpoints), pending: make(chan struct{}, 16)}
	s.connectivity = c
	for _, addr := range s.cfg.Relays {
		if err := c.addRelayLocked(addr, 16); err != nil {
			return err
		}
	}
	var err error
	s.identify, err = p2pproto.New(s.host, p2pproto.Config{AgentVersion: nu.Name + "/" + nu.Version, Addresses: s.publicationAddrs, OnPeerRecord: c.learn})
	if err != nil {
		return err
	}
	if s.cfg.Private || s.cfg.Isolated {
		return nil
	}
	if err := c.initNetwork(); err != nil {
		return err
	}
	c.client, err = circuit.NewClient(s.host, circuit.ClientConfig{AcceptDatagram: c.acceptDatagramPolicy})
	if err != nil {
		return err
	}
	homes, err := homerelay.New(ctx, homerelay.Config{Host: s.host, Client: c.client, Relays: s.cfg.Relays, WSSOptions: s.cfg.WSS.options, Ping: s.identify.Ping, Changed: s.identify.AddressesChanged})
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.homes = homes
	c.mu.Unlock()
	if err := homerelay.ServeTable(s.host, homes.Table); err != nil {
		return err
	}
	c.spawn(c.accept)
	c.spawn(c.acceptDatagrams)
	c.spawn(c.prune)
	return c.runNetwork()
}

func (c *connectivity) spawn(fn func()) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ctx.Err() != nil {
		return false
	}
	c.wg.Add(1)
	go func() { defer c.wg.Done(); fn() }()
	return true
}
func (c *connectivity) close() {
	c.closeOnce.Do(c.shutdown)
}
func (c *connectivity) shutdown() {
	c.mu.Lock()
	c.closed = true
	c.cancel()
	c.mu.Unlock()
	if c.network != nil {
		c.network.close()
	}
	if c.homes != nil {
		c.homes.Close()
	}
	if c.client != nil {
		c.client.Close()
	}
	c.wg.Wait()
	if c.network != nil && c.network.mapper != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = c.network.mapper.Release(ctx)
		cancel()
	}
}
func (c *connectivity) peer(id wire.PeerID) (*pathPeer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, net.ErrClosed
	}
	if p := c.peers[id]; p != nil {
		p.touched = time.Now()
		return p, nil
	}
	if len(c.peers) >= 128 {
		return nil, host.ErrLimit
	}
	p := &pathPeer{gate: make(chan struct{}, 1), touched: time.Now()}
	c.peers[id] = p
	return p, nil
}
func (p *pathPeer) lock(ctx context.Context) error {
	select {
	case p.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *pathPeer) unlock() { <-p.gate }

// admitHints is called only for explicit user/member/link hints, not arbitrary
// lookup results or remote signed records. Such hints may introduce a known relay
// endpoint. Relays learned later still have to match that exact authorized prefix.
func (c *connectivity) admitHints(id wire.PeerID, addrs []wire.Multiaddr) error {
	if c.s.cfg.Private || c.s.cfg.Isolated {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range addrs {
		if !host.IsCircuitAddress(a) {
			if c.network != nil {
				if addr, ok := literalEndpoint(a); ok {
					c.network.allow(id, addr)
				}
			}
			continue
		}
		a, err := lookup.NormalizeAddress(id, a)
		if err != nil {
			return err
		}
		relay, _, peer, err := circuit.ParseAddress(a)
		if err != nil || peer != id {
			return wire.ErrFormat
		}
		if err := c.addRelayLocked(relay, 32); err != nil {
			return err
		}
	}
	return nil
}
func (c *connectivity) learn(record wire.PeerRecord) {
	if c.s.cfg.Private || c.s.cfg.Isolated {
		return
	}
	p, err := c.peer(record.PeerID)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if record.Seq <= p.seq {
		return
	}
	p.seq = record.Seq
	p.routes = nil
	for _, a := range record.Addresses {
		relay, rid, id, err := circuit.ParseAddress(a)
		if err != nil || id != record.PeerID {
			continue
		}
		if known, ok := c.known[rid]; ok && known.Contains(relay) {
			p.routes = append(p.routes, a)
		}
		if len(p.routes) >= 8 {
			break
		}
	}
}
func (c *connectivity) authorized(addr wire.Multiaddr, id wire.PeerID) (wire.Multiaddr, wire.PeerID, error) {
	relay, rid, peer, err := circuit.ParseAddress(addr)
	if err != nil || peer != id {
		return wire.Multiaddr{}, "", wire.ErrFormat
	}
	c.mu.Lock()
	known, ok := c.known[rid]
	c.mu.Unlock()
	if !ok || !known.Contains(relay) {
		return wire.Multiaddr{}, "", errors.New("swarm: relay is not explicitly configured or known from a link")
	}
	return relay, rid, nil
}

func (c *connectivity) dial(ctx context.Context, id wire.PeerID, addr wire.Multiaddr) (*host.Conn, error) {
	if !host.IsCircuitAddress(addr) {
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return c.s.host.Dial(attempt, addr)
	}
	if c.client == nil {
		return nil, errors.New("swarm: relay paths disabled in private/isolated mode")
	}
	relay, rid, err := c.authorized(addr, id)
	if err != nil {
		return nil, err
	}
	p, err := c.peer(id)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	found := false
	for _, a := range p.routes {
		if a.String() == addr.String() {
			found = true
		}
	}
	if !found && len(p.routes) < 8 {
		p.routes = append(p.routes, addr)
	}
	c.mu.Unlock()
	for ctx.Err() == nil {
		if err := p.lock(ctx); err != nil {
			return nil, err
		}
		if p.leg != nil {
			select {
			case <-p.leg.Done():
				p.leg.Close()
				p.leg = nil
			default:
			}
		}
		err = nil
		started := time.Now()
		preparing := p.leg == nil
		if p.leg == nil {
			_, err = c.dialRelay(ctx, relay)
		}
		attempt, stop := context.WithTimeout(ctx, 2*time.Second)
		if p.leg == nil && err == nil {
			_, p.leg, err = qpath.Dial(attempt, c.socket, c.client, rid, id)
			if err == nil {
				p.relay = rid
				c.watch(id, p, p.leg)
				leg := p.leg
				if c.s.cfg.RelayDatagram {
					c.spawn(func() { c.upgradeDatagrams(id, p, leg, rid) })
				} else {
					c.setDatagram(p, "stream")
				}
			}
		}
		var conn *host.Conn
		if err == nil && p.leg != nil {
			if preparing {
				conn, err = c.reconnectInner(attempt, id)
			} else {
				conn, err = c.inner(attempt, id)
			}
		}
		stop()
		if err != nil {
			if p.lastError != err.Error() {
				c.note(id, "relay dial via %s failed: %v", rid.Base36(), err)
			}
			p.lastError = err.Error()
		} else {
			p.lastError = ""
			p.lastConnected = time.Now()
			if preparing {
				p.setupTime = time.Since(started)
				c.s.metrics.setup.Observe(p.setupTime)
				c.note(id, "relay circuit via %s and inner handshake ready in %v", rid.Base36(), p.setupTime.Round(time.Millisecond))
			}
		}
		p.unlock()
		if err == nil {
			return conn, nil
		}
		// Crossed initial attempts reject both raw legs, never attach mismatched ones.
		// The lower peer retries first. Once authenticated only that peer recovers.
		delay := 20 * time.Millisecond
		if c.s.ID() > id {
			delay = 150 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, ctx.Err()
}

func (c *connectivity) inner(ctx context.Context, id wire.PeerID) (*host.Conn, error) {
	// The lower identity always initiates INNER QUIC, regardless of who opened
	// the raw circuit. This gives both ends one deterministic recovery initiator.
	if c.s.ID() < id {
		a, ok := c.socket.PeerAddr(id)
		if !ok {
			return nil, magicsock.ErrPeer
		}
		addr, err := qpath.LocalAddress(id, a)
		if err != nil {
			return nil, err
		}
		conn, err := c.s.host.Dial(ctx, addr)
		if err == nil && c.network != nil {
			// Control negotiation/probing is background work; the first data stream
			// must not pay an extra disco round trip on the relay path.
			c.spawn(func() {
				setup, cancel := context.WithTimeout(c.ctx, 5*time.Second)
				defer cancel()
				c.network.open(setup, conn)
			})
		}
		if err == nil {
			if p, perr := c.peer(id); perr == nil {
				c.spawn(func() { c.optimizeRelays(id, p) })
			}
		}
		return conn, err
	}
	timer := time.NewTicker(5 * time.Millisecond)
	defer timer.Stop()
	for {
		if conn, ok := c.s.host.Connection(id); ok {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
func (c *connectivity) accept() {
	for {
		circ, err := c.client.Accept(c.ctx)
		if err != nil {
			return
		}
		if c.s.cfg.AllowPeer != nil && !c.s.cfg.AllowPeer(circ.Peer) {
			circ.Reset()
			continue
		}
		select {
		case c.pending <- struct{}{}:
		default:
			circ.Reset()
			continue
		}
		if !c.spawn(func() { defer func() { <-c.pending }(); c.acceptOne(circ) }) {
			<-c.pending
			circ.Reset()
		}
	}
}
func (c *connectivity) acceptOne(circ *circuit.Circuit) {
	p, err := c.peer(circ.Peer)
	if err != nil {
		circ.Reset()
		return
	}
	// Never wait behind our own outbound negotiation: reject crossed attempts so
	// both proposers receive failure, then the deterministic retry stagger resolves.
	select {
	case p.gate <- struct{}{}:
	default:
		circ.Reset()
		return
	}
	defer p.unlock()
	if p.leg != nil {
		select {
		case <-p.leg.Done():
			p.leg.Close()
			p.leg = nil
		default:
			circ.Reset()
			return
		}
	}
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	_, leg, err := qpath.Accept(ctx, c.socket, circ)
	if err != nil {
		return
	}
	p.leg = leg
	p.relay = circ.Relay
	c.setDatagram(p, "stream") // the dialer upgrades; relay offers arrive via acceptDatagrams
	c.watch(circ.Peer, p, leg)
	if _, err = c.reconnectInner(ctx, circ.Peer); err != nil {
		p.lastError = err.Error()
		c.note(circ.Peer, "accepted relay circuit via %s but the inner handshake failed: %v", circ.Relay.Base36(), err)
		leg.Close()
	} else {
		p.lastConnected = time.Now()
		c.note(circ.Peer, "accepted relay circuit via %s", circ.Relay.Base36())
	}
}
func (c *connectivity) watch(id wire.PeerID, p *pathPeer, leg *magicsock.Relay) {
	c.spawn(func() {
		select {
		case <-c.ctx.Done():
			leg.Close()
			return
		case <-leg.Done():
			leg.Close()
		}
		if err := p.lock(c.ctx); err != nil {
			return
		}
		if p.leg != leg {
			p.unlock()
			return
		}
		p.leg = nil
		c.note(id, "relay leg via %s closed; re-dial known routes (lower peer first)", p.relay.Base36())
		p.unlock()
		// Prefer the lower peer, but the higher peer must also repair a failed
		// leg: a rebooted lower peer has forgotten the old session and routes.
		if c.s.ID() > id {
			timer := time.NewTimer(300 * time.Millisecond)
			select {
			case <-c.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		for c.ctx.Err() == nil {
			conn, ok := c.s.host.Connection(id)
			if !ok || !c.s.host.IsLocalOnlyAddress(conn.RemoteAddr()) {
				return
			}
			c.mu.Lock()
			routes := append([]wire.Multiaddr(nil), p.routes...)
			c.mu.Unlock()
			for _, addr := range routes {
				setup, cancel := context.WithTimeout(c.ctx, c.recoveryBudget(addr))
				_, err := c.dial(setup, id, addr)
				cancel()
				if err == nil {
					return
				}
			}
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-c.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	})
}
