package swarm

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/p2p/disco"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// NATProfiler supplies the NAT profile that status/doctor shows (D11c §1).
// Returning nil falls back to the estimate from identify observations. Config's
// default implementation adapts the swarm's netcheck monitor.
type NATProfiler interface {
	NATProfile() *apiv0.NATProfile
}

// netMetrics are the connectivity counters behind /metrics (D16). Everything is
// updated from hot paths without locks; histograms are fixed-bucket.
type netMetrics struct {
	switchesDirect, switchesRelay              atomic.Uint64
	probesSent, probesReceived, probesRejected atomic.Uint64
	fetches, fetchFailures                     atomic.Uint64
	timeToDirect, setup, firstByte             *node.Histogram
}

func newNetMetrics() *netMetrics {
	return &netMetrics{timeToDirect: node.NewHistogram(node.DurationBuckets), setup: node.NewHistogram(node.DurationBuckets), firstByte: node.NewHistogram(node.DurationBuckets)}
}

const historyLimit, historyPeers = 12, 128

// note appends to a peer's bounded path history. The history map has its own
// lock and no other lock is taken while holding it, so disco's observer (called
// under the disco lock) and the dial/accept paths can all record events safely.
func (c *connectivity) note(id wire.PeerID, format string, args ...any) {
	c.historyMu.Lock()
	defer c.historyMu.Unlock()
	if c.history == nil {
		c.history = make(map[wire.PeerID][]apiv0.PathEvent)
	}
	h := c.history[id]
	if h == nil && len(c.history) >= historyPeers {
		var oldest wire.PeerID
		var when time.Time
		for peer, events := range c.history {
			if last := events[len(events)-1].Time; oldest == "" || last.Before(when) {
				oldest, when = peer, last
			}
		}
		delete(c.history, oldest)
	}
	if len(h) == historyLimit {
		copy(h, h[1:])
		h = h[:historyLimit-1]
	}
	c.history[id] = append(h, apiv0.PathEvent{Time: time.Now(), Event: fmt.Sprintf(format, args...)})
}
func (c *connectivity) events(id wire.PeerID) []apiv0.PathEvent {
	c.historyMu.Lock()
	defer c.historyMu.Unlock()
	return append([]apiv0.PathEvent(nil), c.history[id]...)
}
func (c *connectivity) forget(id wire.PeerID) {
	c.historyMu.Lock()
	delete(c.history, id)
	c.historyMu.Unlock()
}

// observe is the disco diagnostic hook: counters for /metrics and per-session
// history for `nu doctor`. It runs under the disco lock and takes only atomics and
// the history lock, never the connectivity lock.
func (c *connectivity) observe(e disco.Event) {
	m := c.s.metrics
	switch e.Kind {
	case disco.ProbeSent:
		m.probesSent.Add(1)
	case disco.ProbeReceived:
		m.probesReceived.Add(1)
	case disco.ProbeRejected:
		m.probesRejected.Add(1)
	case disco.PathDirect:
		m.switchesDirect.Add(1)
		m.timeToDirect.Observe(e.Elapsed)
		c.note(e.Peer, "direct path %s selected %v after the probe session started", e.Addr, e.Elapsed.Round(time.Millisecond))
	case disco.PathRelay:
		m.switchesRelay.Add(1)
		c.note(e.Peer, "direct path %s revoked (probes stopped answering or the network changed); traffic falls back to the relay", e.Addr)
	case disco.Candidates:
		c.note(e.Peer, "peer offered %d candidate endpoints, %d admitted by local policy", e.Offered, e.Candidates)
	}
}

// fetchObserved wraps a fetch progress callback to measure time to first byte.
func (s *Swarm) fetchObserved(start time.Time, progress func(done, total int64)) func(done, total int64) {
	first := false
	return func(done, total int64) {
		if !first && done > 0 {
			first = true
			s.metrics.firstByte.Observe(time.Since(start))
		}
		if progress != nil {
			progress(done, total)
		}
	}
}

// Metrics implements node.MetricsNode.
func (s *Swarm) Metrics(_ context.Context, m *node.Metrics) {
	mt := s.metrics
	m.Gauge("nu_net_peers_connected", "Authenticated peer connections.", "", float64(len(s.Connected())))
	status := s.ConnectivityStatus()
	for _, state := range []string{"unknown", "private", "public", "disabled"} {
		m.Gauge("nu_net_reachability", "1 for the current AutoNAT reachability state.", node.Labels("state", state), b2f(status.Reachability == state))
	}
	byPath := map[string]int{}
	for _, p := range status.Paths {
		byPath[p.Path]++
	}
	for _, path := range []string{"direct-quic", "direct", "relay", "recovering", "offline"} {
		m.Gauge("nu_net_paths", "Peer sessions by current path.", node.Labels("path", path), float64(byPath[path]))
	}
	now := time.Now()
	for _, r := range status.Relays {
		labels := node.Labels("relay", r.PeerID, "role", r.Role)
		m.Gauge("nu_net_relay_reserved", "1 while a reservation is held at the relay.", labels, b2f(r.Reserved))
		uptime := 0.0
		if r.Reserved && !r.HeldSince.IsZero() {
			uptime = now.Sub(r.HeldSince).Seconds()
		}
		m.Gauge("nu_net_relay_reservation_uptime_seconds", "Seconds the current reservation has been held without interruption.", labels, uptime)
		m.Gauge("nu_net_relay_rtt_seconds", "Last measured round trip to the relay.", labels, r.RTTMillis/1000)
	}
	if c := s.connectivity; c != nil && c.socket != nil {
		st := c.socket.Stats()
		for _, row := range []struct {
			path, dir string
			v         uint64
		}{{"direct", "sent", st.Sent.Direct}, {"relay", "sent", st.Sent.Relay}, {"outer", "sent", st.Sent.Outer}, {"direct", "received", st.Recv.Direct}, {"relay", "received", st.Recv.Relay}, {"outer", "received", st.Recv.Outer}} {
			m.Counter("nu_net_bytes_total", "UDP payload bytes on the shared socket: direct (inner packets on a proven direct path), relay (inner packets carried by a relay leg) or outer (everything else, including the relay carrier and wire-compatible QUIC).", node.Labels("path", row.path, "direction", row.dir), float64(row.v))
		}
		m.Counter("nu_net_packets_dropped_total", "Packets dropped by the shared socket (queue full, dead relay, unauthorized source).", "", float64(st.Dropped))
	}
	m.Counter("nu_net_path_switches_total", "Path selections: to a direct endpoint, or back to the relay.", node.Labels("to", "direct"), float64(mt.switchesDirect.Load()))
	m.Counter("nu_net_path_switches_total", "Path selections: to a direct endpoint, or back to the relay.", node.Labels("to", "relay"), float64(mt.switchesRelay.Load()))
	for _, row := range []struct {
		outcome string
		v       uint64
	}{{"sent", mt.probesSent.Load()}, {"received", mt.probesReceived.Load()}, {"rejected", mt.probesRejected.Load()}} {
		m.Counter("nu_net_probes_total", "Authenticated path probes by outcome.", node.Labels("outcome", row.outcome), float64(row.v))
	}
	m.Histogram("nu_net_time_to_direct_seconds", "Time from probe session start to the first direct path.", "", mt.timeToDirect.Snapshot())
	m.Histogram("nu_net_session_setup_seconds", "Relay circuit plus inner handshake setup time.", "", mt.setup.Snapshot())
	m.Histogram("nu_fetch_first_byte_seconds", "Time from fetch start to the first verified byte.", "", mt.firstByte.Snapshot())
	m.Counter("nu_fetch_total", "Fetch operations by result.", node.Labels("result", "ok"), float64(mt.fetches.Load()-mt.fetchFailures.Load()))
	m.Counter("nu_fetch_total", "Fetch operations by result.", node.Labels("result", "error"), float64(mt.fetchFailures.Load()))
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// diagnostics fills the doctor-facing sections of a ConnectivityStatus.
func (c *connectivity) diagnostics(out *apiv0.ConnectivityStatus) {
	s := c.s
	if c.network == nil {
		return // private/isolated: no public connectivity to diagnose
	}
	// UDP: the relays are the only endpoints a NAT'd node knows it should reach.
	udp := &apiv0.UDPStatus{Outbound: "unknown"}
	failing := 0
	for _, addr := range s.cfg.Relays {
		id, err := circuit.RelayPeer(addr)
		if err != nil {
			continue
		}
		udp.RelaysConfigured++
		if _, ok := s.host.Connection(id); ok {
			udp.RelaysConnected++
		}
	}
	for _, r := range out.Relays {
		if r.LastError != "" {
			failing++
		}
	}
	switch {
	case udp.RelaysConnected > 0:
		udp.Outbound = "ok"
		udp.Detail = fmt.Sprintf("%d of %d configured relays connected over UDP", udp.RelaysConnected, udp.RelaysConfigured)
	case udp.RelaysConfigured > 0 && failing == udp.RelaysConfigured:
		udp.Outbound = "blocked"
		udp.Detail = fmt.Sprintf("none of the %d configured relays answered over UDP", udp.RelaysConfigured)
	case udp.RelaysConfigured > 0:
		udp.Detail = "relays not reached yet"
	default:
		udp.Detail = "no swarm.relays configured; nothing to test against"
	}
	out.UDP = udp
	// NAT profile: the configured profiler, or the observation-based estimate.
	if s.cfg.NAT != nil {
		out.NAT = s.cfg.NAT.NATProfile()
	}
	if out.NAT == nil {
		out.NAT = c.observedNAT(out.Reachability)
	}
	// Port mapping.
	pm := &apiv0.PortMappingStatus{Enabled: s.cfg.PortMapping != nil}
	if n := c.network; n != nil && n.mapper != nil {
		if m, ok := n.mapper.Candidate(); ok {
			pm.Protocol, pm.External, pm.Expires = string(m.Protocol), m.External.String(), m.Expires
		}
		pm.Error = n.mapper.LastError()
	}
	out.PortMapping = pm
	// IPv6.
	v6 := &apiv0.IPv6Status{}
	for _, a := range s.host.Addrs() {
		if ep, ok := literalEndpoint(a); ok && ep.Addr().Is6() && !ep.Addr().IsLoopback() {
			v6.Listening = true
		}
	}
	for _, ip := range s.cfg.InterfaceIPs() {
		if ip.To4() == nil && ip.IsGlobalUnicast() && !ip.IsPrivate() {
			v6.Global = append(v6.Global, ip.String())
		}
	}
	out.IPv6 = v6
}

// observedNAT estimates mapping behavior from the reflexive addresses that distinct
// observers reported for the primary socket. Observations are not reachability
// proof (D11c); the estimate says so in Source.
func (c *connectivity) observedNAT(reach string) *apiv0.NATProfile {
	p := &apiv0.NATProfile{Mapping: "unknown", Filtering: "unknown", Source: "observations"}
	if reach == "public" {
		p.Mapping, p.Filtering, p.Detail = "none", "none", "two observers confirmed a public address"
	}
	if c.s.identify == nil {
		return p
	}
	bound := c.socket.LocalAddr().(*net.UDPAddr).AddrPort()
	seen := map[netip.Addr]netip.AddrPort{}
	for _, obs := range c.s.identify.Observations() {
		local, ok := literalEndpoint(obs.LocalAddress)
		if !ok || local.Port() != bound.Port() {
			continue
		}
		observer, err := netip.ParseAddrPort(obs.ObserverEndpoint)
		if err != nil {
			continue
		}
		if addr, ok := literalEndpoint(obs.Address); ok {
			seen[observer.Addr().Unmap()] = addr
		}
	}
	p.Observers = len(seen)
	if len(seen) < 2 {
		if p.Mapping == "unknown" {
			p.Detail = "needs reflexive addresses from two observers on different IPs"
		}
		return p
	}
	var addrs []string
	ports := map[uint16]bool{}
	for _, a := range seen {
		addrs = append(addrs, a.String())
		ports[a.Port()] = true
	}
	sort.Strings(addrs)
	switch {
	case len(ports) == 1 && addrs[0] == addrs[len(addrs)-1]:
		if p.Mapping != "none" {
			p.Mapping = "endpoint-independent"
		}
		p.Detail = "all observers saw " + addrs[0]
	default:
		p.Mapping = "endpoint-dependent"
		p.Detail = "observers saw different external ports: " + strings.Join(addrs, ", ") + " (address- or address-and-port-dependent mapping; direct paths need the peer to be EIM, public or mapped)"
	}
	return p
}

// explain gives the plain-language reason a session is not direct.
func explain(row apiv0.PeerPathStatus, st disco.Status, session bool, reach string) string {
	switch row.Path {
	case "direct", "direct-quic", "offline":
		return ""
	}
	switch {
	case !session:
		return "no Nu-path probe session with this peer yet (its control stream has not opened)"
	case st.Candidates == 0:
		return "the peer offered no candidate endpoints this node admits: private/LAN addresses need an explicit swarm.probe_allow grant on both sides, and a NAT'd peer needs a reflexive or router-mapped address"
	case st.Validated == 0 && st.Received == 0:
		return fmt.Sprintf("%d candidate endpoints, %d probes sent, none answered: the peer's NAT filters unsolicited packets (or UDP toward it is blocked); both sides must be EIM, or one public, IPv6 or router-mapped, for a direct path", st.Candidates, st.Sent)
	case st.Validated == 0:
		return fmt.Sprintf("%d candidate endpoints, probes answered but no round trip completed yet (%d received, %d rejected)", st.Candidates, st.Received, st.Rejected)
	default:
		why := "a direct endpoint is proven but not selected yet"
		if reach == "private" {
			why += "; this node is behind a NAT without a public address"
		}
		return why
	}
}

// relayRole labels a relay for status and metrics.
func relayRole(id string, home, backup wire.PeerID) string {
	switch {
	case home != "" && id == home.Base36():
		return "home"
	case backup != "" && id == backup.Base36():
		return "backup"
	}
	return ""
}
