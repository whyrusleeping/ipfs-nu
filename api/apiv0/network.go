package apiv0

import "time"

// ConnectivityStatus is a live socket/path-owner snapshot, absent daemonless.
// Reachability is unknown/private/public/disabled; observations alone are not proof.
type ConnectivityStatus struct {
	Reachability       string           `json:"reachability"`
	Home               string           `json:"home,omitempty"`
	Backup             string           `json:"backup,omitempty"`
	Relays             []RelayStatus    `json:"relays,omitempty"`
	Paths              []PeerPathStatus `json:"paths,omitempty"`
	NetworkChangeError string           `json:"network_change_error,omitempty"`
	PhysicalRebind     bool             `json:"physical_rebind"` // currently false; no no-op success
	// Diagnostics for `nu doctor` (D16): each is nil when the node has no public
	// connectivity owner (private/isolated mode or no host).
	UDP         *UDPStatus         `json:"udp,omitempty"`
	NAT         *NATProfile        `json:"nat,omitempty"`
	PortMapping *PortMappingStatus `json:"port_mapping,omitempty"`
	IPv6        *IPv6Status        `json:"ipv6,omitempty"`
}
type RelayStatus struct {
	Transport string    `json:"transport,omitempty"` // quic-v1 or wss; last selected transport
	UDPError  string    `json:"udp_error,omitempty"` // failed attempt, not proof of blocking
	PeerID    string    `json:"peer_id"`
	Address   string    `json:"address"`
	Role      string    `json:"role,omitempty"` // home, backup or empty (candidate)
	Reserved  bool      `json:"reserved"`       // a reservation is held on a live connection
	RTTMillis float64   `json:"rtt_ms"`
	Expires   time.Time `json:"expires"`
	HeldSince time.Time `json:"held_since,omitempty"` // start of the uninterrupted reservation
	LastError string    `json:"last_error,omitempty"`
}
type PeerPathStatus struct {
	PeerID        string    `json:"peer_id"`
	Path          string    `json:"path"` // direct-quic, direct, relay, relay-wss (outer), recovering, offline
	Relay         string    `json:"relay,omitempty"`
	Direct        string    `json:"direct,omitempty"` // proven physical endpoint, never virtual
	RelayWarm     bool      `json:"relay_warm"`
	LastConnected time.Time `json:"last_connected"`
	LastError     string    `json:"last_error,omitempty"`
	// Probe state of the Nu-path session (zero without one): candidates the peer
	// offered that this node's policy admits, endpoints proven by a probe round trip,
	// probe counters and the time from session start to the first direct path.
	Candidates         int     `json:"candidates,omitempty"`
	Validated          int     `json:"validated,omitempty"`
	ProbesSent         uint64  `json:"probes_sent,omitempty"`
	ProbesReceived     uint64  `json:"probes_received,omitempty"`
	ProbesRejected     uint64  `json:"probes_rejected,omitempty"`
	TimeToDirectMillis float64 `json:"time_to_direct_ms,omitempty"`
	// Why explains, in plain language, why the session is not direct (empty when it is).
	Why string `json:"why,omitempty"`
	// History is the bounded, oldest-first per-session path log.
	History []PathEvent `json:"history,omitempty"`
}

// PathEvent is one per-session path history entry.
type PathEvent struct {
	Time  time.Time `json:"time"`
	Event string    `json:"event"`
}

// UDPStatus says whether UDP reaches the admitted relays, the only reachability test
// a node behind NAT can run on its own. Outbound is ok, blocked or unknown.
type UDPStatus struct {
	Outbound         string `json:"outbound"`
	RelaysConfigured int    `json:"relays_configured"`
	RelaysConnected  int    `json:"relays_connected"`
	Detail           string `json:"detail,omitempty"`
}

// NATProfile is the compact NAT classification (D11c §1). Source names the method:
// "observations" (reflexive addresses reported by connected peers; a basic estimate)
// or a profiler's own name. Mapping/Filtering use RFC 4787 names
// (endpoint-independent, address-dependent, address-and-port-dependent) or
// none (public), unknown.
type NATProfile struct {
	Mapping   string `json:"mapping"`
	Filtering string `json:"filtering"`
	Source    string `json:"source"`
	Observers int    `json:"observers"` // distinct observer IPs behind the estimate
	Detail    string `json:"detail,omitempty"`
}

// PortMappingStatus is the router lease result (UPnP-IGD/NAT-PMP/PCP).
type PortMappingStatus struct {
	Enabled  bool      `json:"enabled"`
	Protocol string    `json:"protocol,omitempty"`
	External string    `json:"external,omitempty"`
	Expires  time.Time `json:"expires,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// IPv6Status reports whether this node listens on a global IPv6 address.
type IPv6Status struct {
	Listening bool     `json:"listening"`
	Global    []string `json:"global,omitempty"` // global unicast addresses on up interfaces
}

// DHTStatus is the Amino DHT section of NetworkStatus (0072). Mode is off (not
// running: private/isolated cluster, swarm.dht = off, or loopback-only listen), client,
// or server (AutoNAT confirmed public reachability). Times are zero until the first
// bootstrap/provide cycle has run; errors are the last cycle's, empty when it succeeded.
type DHTStatus struct {
	Mode         string    `json:"mode"`
	RoutingPeers int       `json:"routing_peers"`
	Bootstrapped time.Time `json:"bootstrapped,omitzero"`
	BootstrapErr string    `json:"bootstrap_error,omitempty"`
	// Provided counts the keys announced in the last provide cycle; Announcing is the
	// address set put into provider records (empty: nothing public to announce).
	Provided    int       `json:"provided"`
	Announcing  []string  `json:"announcing,omitempty"`
	LastProvide time.Time `json:"last_provide,omitzero"`
	ProvideErr  string    `json:"provide_error,omitempty"`
}
