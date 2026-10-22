package repo

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/whyrusleeping/nu"
)

// Default listen addresses. The API and gateway are loopback-only until the API/browser
// security contract lands; the daemon refuses non-loopback values.
const (
	DefaultAPIListen     = "127.0.0.1:5171"
	DefaultGatewayListen = "127.0.0.1:8171"
	// DefaultSwarmPort is the libp2p QUIC UDP port.
	DefaultSwarmPort = 4171
)

// Config is the typed view of config.toml. Keys are dotted TOML paths (`api.listen`);
// `nu config get|set` edits them generically, so unknown keys survive round trips.
type Config struct {
	API     APIConfig     `toml:"api"`
	Gateway GatewayConfig `toml:"gateway"`
	Swarm   SwarmConfig   `toml:"swarm"`
	// Store holds the store budgets (D17 A5).
	Store StoreConfig `toml:"store"`
	// Relay configures `nu relay`; nil when the section is absent (never written by init).
	Relay *RelayConfig `toml:"relay,omitempty"`
	// Public configures the public HTTPS content listener (operations.md P1–P6); nil
	// when the section is absent (never written by init).
	Public *PublicConfig `toml:"public,omitempty"`
	// Follow configures following (0045); nil when the section is absent.
	Follow *FollowConfig `toml:"follow,omitempty"`
	// Cluster holds repair, release and audit settings (0048); nil when absent.
	Cluster *ClusterConfig `toml:"cluster,omitempty"`
}

// ClusterConfig is the `[cluster]` section (docs/cluster.md "Sync, handoff and repair"):
// durations are Go duration strings; empty values keep the defaults. Repair is "auto"
// (default: a daemon holding an admin key is a repair authority) or "off". ReleaseAuto is
// release.auto (default false: releases are reported, applied only by an explicit gc).
type ClusterConfig struct {
	Repair        string `toml:"repair,omitempty"`
	DownAfter     string `toml:"down_after,omitempty"`
	UpAfter       string `toml:"up_after,omitempty"`
	ReleaseAuto   bool   `toml:"release_auto,omitempty"`
	ReleaseSettle string `toml:"release_settle,omitempty"`
	RemoveGrace   string `toml:"remove_grace,omitempty"`
	AuditInterval string `toml:"audit_interval,omitempty"`
	AuditSamples  int    `toml:"audit_samples,omitzero"`
}

// FollowConfig is the `[follow]` section. Jetstream is an optional hint source
// (`wss://jetstream2.us-east.bsky.network/subscribe`): the daemon subscribes only to the
// DIDs it follows and the io.ipfs.nu.name collection, and every event triggers a
// verified re-resolution from the PDS; it is never a feed ingest. Poll is the
// re-resolution backstop (default 1h).
type FollowConfig struct {
	Jetstream string `toml:"jetstream,omitempty"`
	Poll      string `toml:"poll,omitempty"`
}

// PublicConfig is the `[public]` section: the explicitly configured public content
// listener (backlog 0043). It serves only roots marked with `nu public add`, never the
// API, and needs TLS: either cert/key files it serves itself, or `behind_proxy = true`
// with a loopback listen address and a TLS-terminating reverse proxy for the domain.
type PublicConfig struct {
	// Listen is host:port; "" leaves the listener off.
	Listen string `toml:"listen,omitempty"`
	// Domain is the public name, optionally with :port when not 443 (`seed.example`,
	// `seed.example:8443`); sites are served on `<cid>.<domain>`, so a wildcard DNS
	// record and certificate for `*.<domain>` are needed for site origins.
	Domain string `toml:"domain,omitempty"`
	// Cert and Key are PEM files (a full chain and its private key); re-read when they
	// change on disk.
	Cert string `toml:"cert,omitempty"`
	Key  string `toml:"key,omitempty"`
	// BehindProxy serves plain HTTP on a loopback address for a reverse proxy that
	// terminates TLS for Domain and *.Domain on this machine.
	BehindProxy bool `toml:"behind_proxy,omitempty"`
	// MaxRequests and PerIP bound concurrent requests in total and per client address
	// (defaults 256 and 32); excess requests get 503 with Retry-After.
	MaxRequests int `toml:"max_requests,omitzero"`
	PerIP       int `toml:"per_ip,omitzero"`
}

// StoreConfig is the `[store]` section: the physical budgets of D17 A5.
type StoreConfig struct {
	// Limit is the physical byte budget; 0 (default) is unlimited.
	Limit Size `toml:"limit"`
	// FreeReserve is the free space kept on the store's filesystem (default 1G).
	FreeReserve Size `toml:"free_reserve"`
	// GCWatermark is the fraction of Limit above which GC runs (default 0.9).
	GCWatermark float64 `toml:"gc_watermark"`
	// StateReserve is headroom only signed cluster-state writes may use (default 16M,
	// at most an eighth of Limit).
	StateReserve Size `toml:"state_reserve"`
}

// DefaultFreeReserve is the free-space reserve written at init.
const DefaultFreeReserve Size = 1 << 30

// Size is a byte count in config.toml, written as a string with a K, M, G, T or P suffix
// (powers of 1024) and read from such a string or a bare integer, so both
// `nu config set store.limit 50G` and `... 0` work.
type Size int64

// UnmarshalTOML accepts an integer or a size string.
func (z *Size) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case int64:
		if x < 0 {
			return fmt.Errorf("bad size %d", x)
		}
		*z = Size(x)
		return nil
	case string:
		n, err := ParseSize(x)
		*z = Size(n)
		return err
	}
	return fmt.Errorf("bad size %v (want bytes with an optional K/M/G/T/P suffix)", v)
}

// MarshalText writes the shortest exact suffix form ("1G", "1536M", "123").
func (z Size) MarshalText() ([]byte, error) { return []byte(z.String()), nil }

func (z Size) String() string {
	n := int64(z)
	for i := 4; i >= 0; i-- {
		if m := int64(1) << (10 * (i + 1)); n != 0 && n%m == 0 {
			return fmt.Sprintf("%d%c", n/m, "KMGTP"[i])
		}
	}
	return strconv.FormatInt(n, 10)
}

// ParseSize parses a byte count with an optional K, M, G, T or P suffix (powers of 1024);
// "" is 0.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	if i := strings.IndexByte("KMGTP", strings.ToUpper(s[len(s)-1:])[0]); i >= 0 {
		mult = int64(1) << (10 * (i + 1))
		s = strings.TrimSpace(s[:len(s)-1])
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("bad size %q (want bytes with an optional K/M/G/T/P suffix)", s)
	}
	return v * mult, nil
}

// RelayConfig configures the standalone `nu relay` role (networking D10). Flags of the
// same name override these. Limits of 0 use the circuit server defaults.
type RelayConfig struct {
	// Listen holds QUIC multiaddrs; default /ip4/0.0.0.0/udp/443/quic-v1.
	Listen []string `toml:"listen,omitempty"`
	// Advertise holds the public addresses handed to reserving peers (without /p2p);
	// required when a listen address is a wildcard IP.
	Advertise []string `toml:"advertise,omitempty"`
	// Admit lists peer IDs that get full-service (unlimited) circuits with each other.
	Admit []string `toml:"admit,omitempty"`
	// Limits mirror circuit.ServerConfig.
	MaxReservations   int    `toml:"max_reservations,omitzero"`
	ReservationsPerIP int    `toml:"reservations_per_ip,omitzero"`
	MaxCircuits       int    `toml:"max_circuits,omitzero"`
	CircuitsPerPeer   int    `toml:"circuits_per_peer,omitzero"`
	CircuitsPerIP     int    `toml:"circuits_per_ip,omitzero"`
	ReservationTTL    string `toml:"reservation_ttl,omitempty"` // Go duration, default 1h
}

// APIConfig configures the local API listener.
type APIConfig struct {
	// Listen is host:port; must be loopback.
	Listen string `toml:"listen"`
}

// GatewayConfig configures the local gateway listener.
type GatewayConfig struct {
	// Listen is host:port; must be loopback.
	Listen string `toml:"listen"`
}

// SwarmConfig configures libp2p listening and discovery.
type SwarmConfig struct {
	// Listen holds multiaddrs, by default QUIC v1 on UDP DefaultSwarmPort for IPv4 and IPv6.
	Listen []string `toml:"listen"`
	// MDNS enables LAN peer discovery (default true).
	MDNS bool `toml:"mdns"`
	// Public connectivity is opt-in to known relays, never an inferred directory.
	Relays []string `toml:"relays,omitempty"`
	// Mode: public (default), private (explicit peers only), isolated (no public mechanisms).
	Mode  string   `toml:"mode,omitempty"`
	Peers []string `toml:"peers,omitempty"`
	// ProbeAllow entries are peerID@IP:port exact independent destination grants.
	// A grant also permits private candidate disclosure to that peer.
	ProbeAllow     []string `toml:"probe_allow,omitempty"`
	PortmapGateway string   `toml:"portmap_gateway,omitempty"`
	// PeerRelayAllow makes this public node a bounded circuit relay for exactly
	// these PeerIDs (an owner's devices or an allow-list); empty disables it.
	PeerRelayAllow []string `toml:"peer_relay_allow,omitempty"`
	// RelayDatagram: "off" (default) or "on". On, this node upgrades relayed legs it
	// dials to the Nu datagram relay extension and accepts such upgrades from peers;
	// off keeps every relayed leg on the circuit stream (D11c §5: datagram legs lost
	// under random loss, so they stay opt-in until a loss-aware policy exists).
	RelayDatagram string `toml:"relay_datagram,omitempty"`
	HardNAT       string `toml:"hard_nat,omitempty"` // auto (default) or off
	// GSO: auto (default; UDP segmentation offload only when the uplink NIC does it in
	// hardware), on or off (p2p/host.ConfigureGSO).
	GSO string `toml:"gso,omitempty"`
	// DHT: auto (default; Amino client when a non-loopback address is bound, server
	// once AutoNAT confirms public reachability), client (always a client, never a
	// server) or off. Private/isolated modes never run it.
	DHT string `toml:"dht,omitempty"`
	// LookupDomain is a dnsaddr peer domain (`_dnsaddr.<peer>.<domain>`) and RoutingURL a
	// routing/v1 base URL, both public peer-address sources (D13); none by default.
	LookupDomain string `toml:"lookup_domain,omitempty"`
	RoutingURL   string `toml:"routing_url,omitempty"`
}

// DefaultConfig returns the configuration written at init.
func DefaultConfig() *Config {
	return &Config{
		API:     APIConfig{Listen: DefaultAPIListen},
		Gateway: GatewayConfig{Listen: DefaultGatewayListen},
		Swarm: SwarmConfig{Listen: []string{
			fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", DefaultSwarmPort),
			fmt.Sprintf("/ip6/::/udp/%d/quic-v1", DefaultSwarmPort),
		}, MDNS: true},
		Store: StoreConfig{FreeReserve: DefaultFreeReserve, GCWatermark: 0.9, StateReserve: 16 << 20},
	}
}

// Config reads and decodes config.toml over the defaults, so absent keys keep them.
func (r *Repo) Config() (*Config, error) {
	cfg := DefaultConfig()
	if _, err := toml.DecodeFile(r.Path(ConfigFile), cfg); err != nil {
		return nil, fmt.Errorf("read %s: %w", r.Path(ConfigFile), err)
	}
	return cfg, nil
}

// ConfigAll returns the raw configuration tree as nested maps.
func (r *Repo) ConfigAll() (map[string]any, error) {
	m := map[string]any{}
	if _, err := toml.DecodeFile(r.Path(ConfigFile), &m); err != nil {
		return nil, fmt.Errorf("read %s: %w", r.Path(ConfigFile), err)
	}
	return m, nil
}

// ErrConfigKey is returned (wrapped) for a key that does not exist or is malformed.
var ErrConfigKey = errors.New("config key")

// ConfigGet returns the value at a dotted key, e.g. "api.listen". Intermediate tables
// are returned as maps.
func (r *Repo) ConfigGet(key string) (any, error) {
	m, err := r.ConfigAll()
	if err != nil {
		return nil, err
	}
	parts, err := splitKey(key)
	if err != nil {
		return nil, err
	}
	var cur any = m
	for _, p := range parts {
		t, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w %q: %q is not a table", ErrConfigKey, key, p)
		}
		cur, ok = t[p]
		if !ok {
			return nil, fmt.Errorf("%w %q: not set", ErrConfigKey, key)
		}
	}
	return cur, nil
}

// ConfigSet stores a value at a dotted key, creating intermediate tables. The value is
// parsed as a TOML value (`30`, `true`, `["a", "b"]`, `"quoted"`); anything that does not
// parse is stored as a string, so `nu config set api.listen 127.0.0.1:5171` works.
func (r *Repo) ConfigSet(key, raw string) error {
	m, err := r.ConfigAll()
	if err != nil {
		return err
	}
	parts, err := splitKey(key)
	if err != nil {
		return err
	}
	value := ParseValue(raw)
	cur := m
	for _, p := range parts[:len(parts)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			if _, exists := cur[p]; exists {
				return fmt.Errorf("%w %q: %q is not a table", ErrConfigKey, key, p)
			}
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	last := parts[len(parts)-1]
	if _, isTable := cur[last].(map[string]any); isTable {
		return fmt.Errorf("%w %q: is a table, set one of its keys", ErrConfigKey, key)
	}
	cur[last] = value
	return r.writeConfigMap(m)
}

// ParseValue parses a config value given on the command line: a TOML value if it parses,
// else the literal string.
func ParseValue(raw string) any {
	var holder struct {
		V any `toml:"v"`
	}
	if _, err := toml.Decode("v = "+raw, &holder); err == nil && holder.V != nil {
		return holder.V
	}
	return raw
}

func splitKey(key string) ([]string, error) {
	if key == "" {
		return nil, fmt.Errorf("%w: empty", ErrConfigKey)
	}
	parts := strings.Split(key, ".")
	for _, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("%w %q: empty segment", ErrConfigKey, key)
		}
	}
	return parts, nil
}

var configHeader = "# " + nu.Name + " configuration. Keys are dotted TOML paths; edit with `" +
	nu.Name + " config set <key> <value>`.\n\n"

func (r *Repo) writeConfigStruct(cfg *Config) error {
	var buf bytes.Buffer
	buf.WriteString(configHeader)
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(cfg); err != nil {
		return err
	}
	return writeFileAtomic(r.Path(ConfigFile), buf.Bytes())
}

func (r *Repo) writeConfigMap(m map[string]any) error {
	var buf bytes.Buffer
	buf.WriteString(configHeader)
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(m); err != nil {
		return err
	}
	return writeFileAtomic(r.Path(ConfigFile), buf.Bytes())
}

// FlattenConfig returns dotted key/value pairs in sorted order, for display.
func FlattenConfig(m map[string]any) []KeyValue {
	var out []KeyValue
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		t, ok := v.(map[string]any)
		if !ok {
			out = append(out, KeyValue{Key: prefix, Value: v})
			return
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			walk(p, t[k])
		}
	}
	walk("", m)
	return out
}

// KeyValue is one flattened config entry.
type KeyValue struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}
