package repo

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/whyrusleeping/nu"
)

// Default listen addresses. The API and gateway are loopback-only until the API/browser
// security contract lands; the daemon refuses non-loopback values.
const (
	DefaultAPIListen     = "127.0.0.1:5171"
	DefaultGatewayListen = "127.0.0.1:8171"
	// DefaultSwarmPort is the libp2p QUIC UDP port. Reserved for the networking track.
	DefaultSwarmPort = 4171
)

// Config is the typed view of config.toml. Keys are dotted TOML paths (`api.listen`);
// `nu config get|set` edits them generically, so unknown keys survive round trips.
type Config struct {
	API     APIConfig     `toml:"api"`
	Gateway GatewayConfig `toml:"gateway"`
	Swarm   SwarmConfig   `toml:"swarm"`
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

// SwarmConfig configures libp2p listening. Reserved: read by the networking track.
type SwarmConfig struct {
	// Listen holds multiaddrs, by default QUIC v1 on UDP DefaultSwarmPort for IPv4 and IPv6.
	Listen []string `toml:"listen"`
}

// DefaultConfig returns the configuration written at init.
func DefaultConfig() *Config {
	return &Config{
		API:     APIConfig{Listen: DefaultAPIListen},
		Gateway: GatewayConfig{Listen: DefaultGatewayListen},
		Swarm: SwarmConfig{Listen: []string{
			fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", DefaultSwarmPort),
			fmt.Sprintf("/ip6/::/udp/%d/quic-v1", DefaultSwarmPort),
		}},
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
