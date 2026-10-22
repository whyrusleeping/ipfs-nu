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
	LastConnected time.Time `json:"last_connected"`
	LastError     string    `json:"last_error,omitempty"`
}
