package swarm

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/whyrusleeping/nu/p2p/homerelay"
	"github.com/whyrusleeping/nu/p2p/magicsock"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/proto/qpath"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// D11c §5 relay optimization (0019): the circuit dialer upgrades each new stream
// leg to the Nu datagram extension with backoff, the target attaches relay offers
// only to its live leg for that exact peer/relay, and the inner-QUIC initiator
// (lower PeerID) re-homes the pair onto the relay with the smallest combined RTT
// using both peers' /nu/relays/1 tables. Direct probing continues regardless.

var datagramRetry = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute}
var relayOptimizeInterval = 10 * time.Minute
var relayOptimizeDelay = time.Second

// acceptDatagramPolicy answers a relay's offer: only when this node itself opted
// in (swarm.relay_datagram = on; an "on" dialer cannot upgrade an "off" target)
// and a live stream leg to that peer through that relay exists. It may wait
// briefly for in-progress setup.
func (c *connectivity) acceptDatagramPolicy(relay, peer wire.PeerID) bool {
	if !c.s.cfg.RelayDatagram {
		return false
	}
	c.mu.Lock()
	p := c.peers[peer]
	c.mu.Unlock()
	if p == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
	defer cancel()
	if p.lock(ctx) != nil {
		return false
	}
	defer p.unlock()
	return p.leg != nil && p.relay == relay && relayLive(p.leg)
}
func relayLive(leg *magicsock.Relay) bool {
	select {
	case <-leg.Done():
		return false
	default:
		return true
	}
}
func (c *connectivity) acceptDatagrams() {
	for {
		dl, err := c.client.AcceptDatagram(c.ctx)
		if err != nil {
			return
		}
		c.mu.Lock()
		p := c.peers[dl.Peer]
		c.mu.Unlock()
		if p == nil {
			dl.Close()
			continue
		}
		ctx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
		if p.lock(ctx) != nil {
			cancel()
			dl.Close()
			continue
		}
		cancel()
		leg := p.leg
		if leg == nil || !relayLive(leg) || p.relay != dl.Relay {
			dl.Close()
		} else if err := qpath.AttachDatagram(leg, dl, dl.Relay, dl.Peer); err == nil {
			c.setDatagram(p, "datagram")
			c.watchDatagram(p, leg, dl)
		}
		p.unlock()
	}
}
func (c *connectivity) setDatagram(p *pathPeer, mode string) {
	c.mu.Lock()
	p.datagram = mode
	c.mu.Unlock()
}
func (c *connectivity) watchDatagram(p *pathPeer, leg *magicsock.Relay, dl *circuit.DatagramLeg) {
	c.spawn(func() {
		select {
		case <-c.ctx.Done():
		case <-dl.Done():
			if relayLive(leg) {
				c.setDatagram(p, "stream")
			}
		case <-leg.Done():
		}
	})
}

// upgradeDatagrams runs for the dialer of a stream leg until that leg ends.
func (c *connectivity) upgradeDatagrams(id wire.PeerID, p *pathPeer, leg *magicsock.Relay, relay wire.PeerID) {
	attempt := 0
	c.setDatagram(p, "stream")
	for c.ctx.Err() == nil && relayLive(leg) {
		ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
		dl, err := qpath.UpgradeDatagram(ctx, c.client, leg, relay, id)
		cancel()
		if err == nil {
			attempt = 0
			c.setDatagram(p, "datagram")
			select {
			case <-c.ctx.Done():
				return
			case <-leg.Done():
				return
			case <-dl.Done():
				c.setDatagram(p, "stream")
			}
		} else {
			c.setDatagram(p, "stream: "+err.Error())
		}
		delay := datagramRetry[min(attempt, len(datagramRetry)-1)]
		attempt++
		timer := time.NewTimer(delay)
		select {
		case <-c.ctx.Done():
			timer.Stop()
			return
		case <-leg.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// localRelayTable is this node's table: held reservations plus any configured
// relay with a recent measured RTT (a CONNECT side needs no reservation).
func (c *connectivity) localRelayTable() []homerelay.TableEntry {
	var out []homerelay.TableEntry
	seen := map[wire.PeerID]bool{}
	if c.homes != nil {
		for _, r := range c.homes.Snapshot().Relays {
			if r.RTT > 0 && !seen[r.Peer] {
				seen[r.Peer] = true
				out = append(out, homerelay.TableEntry{Peer: r.Peer, RTT: r.RTT})
			}
		}
	}
	return out
}

// optimizeRelays runs once per peer while an inner connection exists.
func (c *connectivity) optimizeRelays(id wire.PeerID, p *pathPeer) {
	c.mu.Lock()
	if p.optimizing || c.homes == nil {
		c.mu.Unlock()
		return
	}
	p.optimizing = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); p.optimizing = false; c.mu.Unlock() }()
	delay := relayOptimizeDelay
	for c.ctx.Err() == nil {
		timer := time.NewTimer(delay)
		select {
		case <-c.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = relayOptimizeInterval
		conn, ok := c.s.host.Connection(id)
		if !ok || !c.s.host.IsLocalOnlyAddress(conn.RemoteAddr()) {
			return
		}
		if set, ok := c.socket.Paths(id); !ok || set.Selected.IsValid() {
			continue // direct: nothing to optimize now; relay stays warm
		}
		ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
		err := c.optimizeOnce(ctx, id, p)
		cancel()
		if err != nil {
			c.mu.Lock()
			p.relayChoice = err.Error()
			c.mu.Unlock()
		}
	}
}
func (c *connectivity) optimizeOnce(ctx context.Context, id wire.PeerID, p *pathPeer) error {
	remote, err := homerelay.FetchTable(ctx, c.s.host, id)
	if err != nil {
		return err
	}
	// Only relays this node explicitly knows are candidates; measure any that
	// have no recent RTT by dialing them (the CONNECT side needs a connection).
	local := c.localRelayTable()
	have := map[wire.PeerID]bool{}
	for _, e := range local {
		have[e.Peer] = true
	}
	for _, e := range remote {
		if have[e.Peer] || e.RTT <= 0 {
			continue
		}
		c.mu.Lock()
		addr, known := c.known[e.Peer]
		c.mu.Unlock()
		if !known {
			continue
		}
		if _, _, err := addr.Dial(ctx, c.s.host, 2*time.Second); err != nil {
			continue
		}
		rtt, err := c.s.identify.Ping(ctx, e.Peer)
		if err != nil {
			continue
		}
		local = append(local, homerelay.TableEntry{Peer: e.Peer, RTT: rtt})
	}
	if err := p.lock(ctx); err != nil {
		return err
	}
	current := p.relay
	leg := p.leg
	p.unlock()
	if leg == nil || !relayLive(leg) {
		return nil
	}
	best, sum, better := homerelay.Choose(local, remote, current)
	c.mu.Lock()
	addr, known := c.known[best]
	if best != "" {
		p.relayChoice = best.Base36() + " " + sum.String()
	}
	c.mu.Unlock()
	if !better || !known {
		return nil
	}
	route, err := circuit.Address(preferredEndpoint(addr), id)
	if err != nil {
		return err
	}
	err = c.switchRelay(ctx, id, p, leg, route)
	c.s.log.Info("swarm: pair-aware relay switch", "peer", id.Base36(), "from", current.Base36(), "to", best.Base36(), "combined_rtt", sum, "err", err)
	return err
}

// switchRelay closes the current stream leg and re-dials through route. The
// inner QUIC connection is kept (host.Dial reuses it); packets in flight during
// the swap are retransmitted by inner QUIC.
func (c *connectivity) switchRelay(ctx context.Context, id wire.PeerID, p *pathPeer, old *magicsock.Relay, route wire.Multiaddr) error {
	if err := p.lock(ctx); err != nil {
		return err
	}
	if p.leg != old {
		p.unlock()
		return nil
	}
	p.leg = nil
	p.unlock()
	old.Close()
	c.mu.Lock()
	routes := []wire.Multiaddr{route}
	for _, r := range p.routes {
		if r.String() != route.String() {
			routes = append(routes, r)
		}
	}
	p.routes = routes[:min(len(routes), 8)]
	c.mu.Unlock()
	_, err := c.dial(ctx, id, route)
	return err
}

// startPeerRelay serves bounded circuit-v2 relaying (and the datagram extension)
// on this public node for an explicit allow-list only; nothing else may reserve
// or connect. Advertised addresses are this node's literal listen endpoints.
func (s *Swarm) startPeerRelay(key *wire.NodeKey) error {
	allow := s.cfg.PeerRelayAllow
	if len(allow) == 0 || s.cfg.Private || s.cfg.Isolated {
		return nil
	}
	allowed := make(map[wire.PeerID]bool, len(allow))
	for _, id := range allow {
		allowed[id] = true
	}
	var addrs []wire.Multiaddr
	for _, a := range Advertise(s.host.Addrs(), s.cfg.InterfaceIPs) {
		if ep, ok := literalEndpoint(a); !ok || magicsock.IsVirtual(ep) {
			continue
		}
		full, err := wire.ParseMultiaddr(a.String()+"/p2p/"+s.host.ID().String(), 1024)
		if err == nil {
			addrs = append(addrs, full)
		}
		if len(addrs) == circuit.MaxAddresses {
			break
		}
	}
	if len(addrs) == 0 {
		return errors.New("swarm: peer relay needs a literal non-virtual listen address")
	}
	server, err := circuit.NewServer(s.host, key, circuit.ServerConfig{
		Admitted:          allow,
		AllowPeer:         func(id wire.PeerID, _ netip.Addr) bool { return allowed[id] },
		MaxReservations:   len(allow),
		ReservationsPerIP: len(allow), // a household's devices share one NAT address
		MaxCircuits:       64,
		CircuitsPerPeer:   8,
		CircuitsPerIP:     64,
		Addresses:         addrs,
	})
	if err != nil {
		return err
	}
	s.peerRelay = server
	return nil
}

// preferredEndpoint is the admitted QUIC endpoint a pair-aware switch uses.
func preferredEndpoint(e homerelay.Endpoints) wire.Multiaddr {
	return e.QUIC
}
