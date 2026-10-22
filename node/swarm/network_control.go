package swarm

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	ma "github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/nu/p2p/disco"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/magicsock"
	"github.com/whyrusleeping/nu/p2p/netcheck"
	"github.com/whyrusleeping/nu/p2p/netmon"
	"github.com/whyrusleeping/nu/p2p/portmap"
	"github.com/whyrusleeping/nu/p2p/proto/autonat"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
)

var errPhysicalRebind = errors.New("swarm: routes invalidated; physical socket replacement is not implemented (restart the daemon if its bound address disappeared)")

type networkControl struct {
	c            *connectivity
	disco        *disco.Service
	autonat      *autonat.Service
	mapper       *portmap.Client
	monitor      *netmon.Monitor
	pipeline     *netmon.Pipeline
	mu           sync.Mutex
	allowed      map[wire.PeerID]map[netip.AddrPort]bool
	opened       map[*host.Conn]bool
	reach        string
	confirmed    []wire.Multiaddr // only current two-observer AutoNAT proofs
	generation   uint64
	lastReaction netmon.Reaction
	confirm      chan struct{}
	natProfile   *netcheck.Monitor // 0018, see netcheck.go
}

func literalEndpoint(a wire.Multiaddr) (netip.AddrPort, bool) {
	m, err := ma.NewMultiaddrBytes(a.Bytes())
	if err != nil || len(m) < 3 || (m[0].Code() != ma.P_IP4 && m[0].Code() != ma.P_IP6) || m[1].Code() != ma.P_UDP || m[2].Code() != ma.P_QUIC_V1 {
		return netip.AddrPort{}, false
	}
	addr, err := netip.ParseAddrPort(net.JoinHostPort(m[0].Value(), m[1].Value()))
	return addr, err == nil && addr.Port() != 0
}
func (c *connectivity) initNetwork() error {
	n := &networkControl{c: c, allowed: make(map[wire.PeerID]map[netip.AddrPort]bool), opened: make(map[*host.Conn]bool), reach: "unknown", confirm: make(chan struct{}, 1)}
	c.mu.Lock()
	c.network = n
	c.mu.Unlock()
	for id, addrs := range c.s.cfg.ProbeAllowed {
		for _, a := range addrs {
			n.allow(id, a)
		}
	}
	var err error
	n.disco, err = disco.New(c.ctx, disco.Config{Host: c.s.host, Socket: c.socket, Candidates: n.candidates, Authorize: n.authorize})
	if err != nil {
		return err
	}
	n.autonat, err = autonat.New(c.s.host, autonat.Config{Timeout: 2 * time.Second, DialTimeout: time.Second})
	if err != nil {
		return err
	}
	if cfg := c.s.cfg.PortMapping; cfg != nil {
		mc := *cfg
		bound := c.socket.LocalAddr().(*net.UDPAddr).AddrPort()
		if !mc.Local.IsValid() {
			local := bound.Addr().Unmap()
			if local.IsUnspecified() {
				route, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(mc.Gateway, 5351)))
				if err != nil {
					return err
				}
				local = route.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
				route.Close()
			}
			mc.Local = netip.AddrPortFrom(local, bound.Port())
		}
		if mc.Local.Port() != bound.Port() || mc.Local.Addr().Is4() != bound.Addr().Unmap().Is4() || (!bound.Addr().IsUnspecified() && mc.Local.Addr() != bound.Addr().Unmap()) {
			return errors.New("swarm: portmap local address must belong to the primary shared socket")
		}
		n.mapper, err = portmap.New(mc)
		if err != nil {
			return err
		}
	}
	return nil
}
func (n *networkControl) allow(id wire.PeerID, addr netip.AddrPort) {
	if !addr.IsValid() || addr.Port() == 0 {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.allowed[id] == nil {
		if len(n.allowed) >= 128 {
			return
		}
		n.allowed[id] = make(map[netip.AddrPort]bool)
	}
	if len(n.allowed[id]) < 32 {
		n.allowed[id][netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())] = true
	}
}
func (n *networkControl) authorize(id wire.PeerID, c disco.Candidate) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.allowed[id][c.Addr]
}
func (n *networkControl) candidates(id wire.PeerID) []disco.Candidate {
	n.mu.Lock()
	trusted := len(n.allowed[id]) > 0
	n.mu.Unlock()
	bound := n.c.socket.LocalAddr().(*net.UDPAddr).AddrPort()
	now := time.Now()
	var list []disco.Candidate
	if bound.Addr().IsUnspecified() {
		list, _ = disco.Gather(bound.Port(), now)
	} else {
		list = append(list, disco.Candidate{Kind: disco.LAN, Addr: bound, Expires: now.Add(10 * time.Minute)})
	}
	for _, obs := range n.c.s.identify.Observations() {
		local, ok := literalEndpoint(obs.LocalAddress)
		if !ok || local != bound || now.Sub(obs.Updated) > 10*time.Minute {
			continue
		}
		addr, ok := literalEndpoint(obs.Address)
		if ok {
			list = append(list, disco.Candidate{Kind: disco.Reflexive, Addr: addr, Expires: obs.Updated.Add(10 * time.Minute)})
		}
	}
	if n.mapper != nil {
		if m, ok := n.mapper.Candidate(); ok && !m.Remote.IsValid() {
			if c, ok := disco.FromMapping(m, netip.AddrPort{}, now); ok {
				list = append(list, c)
			}
		}
	}
	out := make([]disco.Candidate, 0, disco.MaxCandidates)
	seen := map[netip.AddrPort]bool{}
	for _, c := range list {
		a := c.Addr.Addr().Unmap()
		c.Addr = netip.AddrPortFrom(a, c.Addr.Port())
		// No LAN/loopback disclosure to arbitrary relayed public identities. An
		// independent exact-endpoint grant creates an explicit peer relationship.
		if a.Is4() != bound.Addr().Unmap().Is4() || a.IsUnspecified() || a.IsMulticast() || a.IsLinkLocalUnicast() || magicsock.IsVirtual(c.Addr) || !c.Expires.After(now) || seen[c.Addr] {
			continue
		}
		if (a.IsPrivate() || a.IsLoopback()) && !trusted {
			continue
		}
		seen[c.Addr] = true
		out = append(out, c)
		if len(out) == disco.MaxCandidates {
			break
		}
	}
	return out
}
func (n *networkControl) open(ctx context.Context, conn *host.Conn) {
	if !n.c.s.host.IsLocalOnlyAddress(conn.RemoteAddr()) {
		return
	}
	_, active := n.disco.Status(conn.PeerID())
	n.mu.Lock()
	for old := range n.opened {
		if old.Context().Err() != nil {
			delete(n.opened, old)
		}
	}
	if pending, known := n.opened[conn]; active || known && pending {
		n.mu.Unlock()
		return
	}
	n.opened[conn] = true // pending; false below means a completed control setup
	n.mu.Unlock()
	err := n.disco.Open(ctx, conn)
	n.mu.Lock()
	if err != nil {
		delete(n.opened, conn)
	} else {
		n.opened[conn] = false
	}
	n.mu.Unlock()
}

func (n *networkControl) ensureControl() {
	for _, conn := range n.c.s.host.Connections() {
		if n.c.s.ID() >= conn.PeerID() || !n.c.s.host.IsLocalOnlyAddress(conn.RemoteAddr()) {
			continue
		}
		n.c.spawn(func() { ctx, cancel := context.WithTimeout(n.c.ctx, 5*time.Second); defer cancel(); n.open(ctx, conn) })
	}
}

func (c *connectivity) runNetwork() error {
	n := c.network
	pipeline, err := netmon.NewPipeline(3 * time.Second)
	if err != nil {
		return err
	}
	n.pipeline = pipeline
	hooks := []netmon.Hook{
		func(ctx context.Context, e netmon.Event) error {
			// Invalidate proofs immediately, before slow address/mapping work. Do not
			// pretend that this reopens/replaces the physical UDP socket.
			err := n.disco.RestartProbing(ctx, e)
			n.mu.Lock()
			n.reach = "unknown"
			n.confirmed = nil
			n.generation++
			n.mu.Unlock()
			c.homes.SetNeeded(true)
			if e.Kind & ^netmon.Resync != 0 {
				return errors.Join(err, errPhysicalRebind)
			}
			return err
		},
		n.confirmAddresses,
		func(ctx context.Context, _ netmon.Event) error {
			if n.mapper == nil {
				return nil
			}
			_, err := n.mapper.Refresh(ctx)
			n.disco.Update()
			return err
		},
		func(ctx context.Context, _ netmon.Event) error { return c.homes.Renew(ctx) },
		func(ctx context.Context, _ netmon.Event) error {
			c.s.identify.AddressesChanged()
			var errs []error
			for _, conn := range c.s.host.Connections() {
				if err := c.s.identify.Push(ctx, conn.PeerID()); err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		},
		func(ctx context.Context, e netmon.Event) error {
			err := n.disco.RestartProbing(ctx, e)
			n.ensureControl()
			return err
		},
	}
	for stage, hook := range hooks {
		if _, err := pipeline.Register(netmon.Stage(stage), hook); err != nil {
			return err
		}
	}
	if err := c.startNetcheck(pipeline); err != nil { // 0018 NAT profile
		return err
	}
	mon, err := netmon.Start(c.ctx, netmon.Options{})
	if err != nil {
		return err
	}
	n.monitor = mon
	events, unsubscribe := mon.Subscribe()
	c.spawn(func() { defer unsubscribe(); _ = pipeline.Run(c.ctx, events) })
	reactions, unsub := pipeline.Subscribe()
	c.spawn(func() {
		defer unsub()
		for {
			select {
			case <-c.ctx.Done():
				return
			case r := <-reactions:
				n.mu.Lock()
				n.lastReaction = r
				n.mu.Unlock()
			}
		}
	})
	if n.mapper != nil {
		c.spawn(func() { _ = n.mapper.Run(c.ctx) })
		c.spawn(func() {
			for {
				select {
				case <-c.ctx.Done():
					return
				case <-n.mapper.Changes():
					n.disco.Update()
				}
			}
		})
	}
	c.spawn(func() {
		timer := time.NewTicker(30 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-timer.C:
				ctx, cancel := context.WithTimeout(c.ctx, 3*time.Second)
				_ = n.confirmAddresses(ctx, netmon.Event{})
				cancel()
				n.disco.Update()
			}
		}
	})
	return nil
}
func (n *networkControl) confirmAddresses(ctx context.Context, _ netmon.Event) error {
	select {
	case n.confirm <- struct{}{}:
		defer func() { <-n.confirm }()
	case <-ctx.Done():
		return ctx.Err()
	}
	n.mu.Lock()
	generation := n.generation
	n.mu.Unlock()
	// Two independent observer IPs must verify the SAME shared-socket candidate.
	// Observations/mappings alone, a single relay, or two IDs at one IP never
	// disable reservations. Unknown keeps home+backup warm.
	proofs := map[netip.AddrPort]map[netip.Addr]bool{}
	var errs []error
	checked, failed := 0, 0
	for _, relay := range n.c.s.cfg.Relays {
		id, err := circuit.RelayPeer(relay)
		if err != nil {
			continue
		}
		if _, ok := n.c.s.host.Connection(id); !ok {
			continue
		}
		var reqs []autonat.Request
		for _, candidate := range n.candidates("") {
			ip := candidate.Addr.Addr()
			if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
				continue
			}
			addr, err := host.UDPAddress(net.UDPAddrFromAddrPort(candidate.Addr))
			if err == nil {
				reqs = append(reqs, autonat.Request{Address: addr})
			}
			if len(reqs) == 8 {
				break
			}
		}
		if len(reqs) == 0 {
			continue
		}
		result, err := n.autonat.Check(ctx, id, reqs)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		checked++
		if result.Reachability == autonat.Unreachable {
			failed++
			continue
		}
		if result.Reachability != autonat.Reachable {
			continue
		}
		addr, ok := literalEndpoint(result.Address)
		observer, err := netip.ParseAddrPort(result.Endpoint)
		if !ok || err != nil {
			continue
		}
		if proofs[addr] == nil {
			proofs[addr] = map[netip.Addr]bool{}
		}
		proofs[addr][observer.Addr().Unmap()] = true
	}
	confirmed := confirmedAddresses(proofs)
	reach := "unknown"
	if len(confirmed) > 0 {
		reach = "public"
	}
	if checked > 0 && failed == checked {
		reach = "private"
	}
	n.mu.Lock()
	if generation != n.generation {
		n.mu.Unlock()
		return errors.New("swarm: network changed during reachability confirmation; retaining relays")
	}
	changed := !sameAddresses(n.confirmed, confirmed)
	n.confirmed = confirmed
	n.reach = reach
	n.mu.Unlock()
	if changed {
		n.c.s.identify.AddressesChanged()
	}
	n.c.s.dhtReachable(reach == "public")
	n.c.mu.Lock()
	homes := n.c.homes
	n.c.mu.Unlock()
	if homes != nil {
		homes.SetNeeded(reach != "public")
	}
	return errors.Join(errs...)
}

// Proof aggregation is deliberately conservative: independent IPs, the same
// candidate, and no private/virtual endpoints. A mapping/observation isn't proof.
func confirmedAddresses(proofs map[netip.AddrPort]map[netip.Addr]bool) []wire.Multiaddr {
	var out []wire.Multiaddr
	for endpoint, servers := range proofs {
		ip := endpoint.Addr().Unmap()
		if len(servers) < 2 || endpoint.Port() == 0 || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || magicsock.IsVirtual(endpoint) {
			continue
		}
		if addr, err := host.UDPAddress(net.UDPAddrFromAddrPort(endpoint)); err == nil {
			out = append(out, addr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}
func sameAddresses(a, b []wire.Multiaddr) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].String() != b[i].String() {
			return false
		}
	}
	return true
}

func (n *networkControl) close() {
	n.mu.Lock()
	n.confirmed = nil
	n.reach = "disabled"
	n.mu.Unlock()
	if n.monitor != nil {
		n.monitor.Close()
	}
	if n.disco != nil {
		n.disco.Close()
	}
	if n.autonat != nil {
		n.autonat.Close()
	}
}
