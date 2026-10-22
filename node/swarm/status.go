package swarm

import (
	"sort"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/p2p/disco"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// DisablePublic is the one-way runtime privacy transition used when a running
// node joins/initializes a private or isolated cluster. It joins public workers,
// removes STOP/path/AutoNAT handlers and closes multicast/public lookup. Explicit
// direct sessions and the cluster's dynamic membership gate remain in force.
// Re-enabling public networking requires a restart, not an implicit gate clear.
func (s *Swarm) DisablePublic() {
	if s.connectivity != nil {
		s.connectivity.close()
	}
	for _, conn := range s.host.Connections() {
		if s.host.IsLocalOnlyAddress(conn.RemoteAddr()) {
			conn.Close()
		}
	}
	if s.lookup != nil {
		s.lookup.Close()
	}
	if s.identify != nil {
		s.identify.AddressesChanged()
	}
}

func (s *Swarm) ConnectivityStatus() *apiv0.ConnectivityStatus {
	out := &apiv0.ConnectivityStatus{Reachability: "disabled"}
	c := s.connectivity
	if c == nil {
		return out
	}
	c.mu.Lock()
	closed := c.closed
	homes := c.homes
	peers := make(map[wire.PeerID]*pathPeer, len(c.peers))
	for id, p := range c.peers {
		peers[id] = p
	}
	c.mu.Unlock()
	if c.network != nil && !closed {
		c.network.mu.Lock()
		out.Reachability = c.network.reach
		if err := c.network.lastReaction.Err; err != nil {
			out.NetworkChangeError = err.Error()
		}
		c.network.mu.Unlock()
	}
	if homes != nil && !closed {
		state := homes.Snapshot()
		if state.Home != "" {
			out.Home = state.Home.Base36()
		}
		if state.Backup != "" {
			out.Backup = state.Backup.Base36()
		}
		for _, r := range state.Relays {
			id := r.Peer.Base36()
			out.Relays = append(out.Relays, apiv0.RelayStatus{PeerID: id, Address: r.Address.String(), Role: relayRole(id, state.Home, state.Backup), Reserved: !r.HeldSince.IsZero() && r.Expires.After(time.Now()), RTTMillis: float64(r.RTT.Microseconds()) / 1000, Expires: r.Expires, HeldSince: r.HeldSince, LastError: r.LastError})
		}
	}
	// Include ordinary wire-compatible peers and outer relay connections too.
	paths := map[wire.PeerID]apiv0.PeerPathStatus{}
	for _, conn := range s.host.Connections() {
		row := apiv0.PeerPathStatus{PeerID: conn.PeerID().Base36(), Path: "recovering"}
		if !s.host.IsLocalOnlyAddress(conn.RemoteAddr()) {
			row.Path = "direct-quic"
			row.Direct = conn.RemoteAddr().String()
		}
		paths[conn.PeerID()] = row
	}
	for id, p := range peers {
		row := apiv0.PeerPathStatus{PeerID: id.Base36(), Path: "offline"}
		if previous, ok := paths[id]; ok {
			row = previous
		}
		if set, ok := c.socket.Paths(id); ok {
			row.RelayWarm = set.Relay
			if conn, live := s.host.Connection(id); live && s.host.IsLocalOnlyAddress(conn.RemoteAddr()) {
				row.Path = "recovering"
				if set.Relay {
					row.Path = "relay"
				}
				if set.Selected.IsValid() {
					row.Path = "direct"
					row.Direct = set.Selected.String()
				}
			} else if previous, ok := paths[id]; ok {
				row = previous
			}
		}
		select {
		case p.gate <- struct{}{}:
			if p.relay != "" {
				row.Relay = p.relay.Base36()
			}
			row.LastError = p.lastError
			row.LastConnected = p.lastConnected
			p.unlock()
		default:
		}
		row.History = c.events(id)
		var st disco.Status
		session := false
		if c.network != nil && !closed && c.network.disco != nil {
			st, session = c.network.disco.Status(id)
		}
		if session {
			row.Candidates, row.Validated = st.Candidates, st.Validated
			row.ProbesSent, row.ProbesReceived, row.ProbesRejected = st.Sent, st.Received, st.Rejected
			row.TimeToDirectMillis = float64(st.TimeToDirect.Microseconds()) / 1000
		}
		row.Why = explain(row, st, session, out.Reachability)
		paths[id] = row
	}
	for _, p := range paths {
		out.Paths = append(out.Paths, p)
	}
	sort.Slice(out.Paths, func(i, j int) bool { return out.Paths[i].PeerID < out.Paths[j].PeerID })
	if !closed {
		c.diagnostics(out)
	}
	return out
}
