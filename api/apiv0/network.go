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
}
type RelayStatus struct {
	PeerID    string    `json:"peer_id"`
	Address   string    `json:"address"`
	RTTMillis float64   `json:"rtt_ms"`
	Expires   time.Time `json:"expires"`
	LastError string    `json:"last_error,omitempty"`
}
type PeerPathStatus struct {
	PeerID        string    `json:"peer_id"`
	Path          string    `json:"path"` // direct-quic, direct, relay, recovering, offline
	Relay         string    `json:"relay,omitempty"`
	Direct        string    `json:"direct,omitempty"` // proven physical endpoint, never virtual
	RelayWarm     bool      `json:"relay_warm"`
	RelayMode     string    `json:"relay_mode,omitempty"`   // stream, datagram, or "stream: <upgrade error>" (0019)
	RelayChoice   string    `json:"relay_choice,omitempty"` // pair-aware best relay and combined RTT, or error
	LastConnected time.Time `json:"last_connected"`
	LastError     string    `json:"last_error,omitempty"`
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
