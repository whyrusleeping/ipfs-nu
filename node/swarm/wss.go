package swarm

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/p2p/wss"
)

// WSSPolicy is operator-only policy, immutable after Start. Pins supplement (never
// replace) Web-PKI verification. Proxy credentials are not exposed in status.
type WSSPolicy struct {
	Options  wss.DialOptions
	CertPins map[wire.PeerID][32]byte
}

func (p WSSPolicy) options(id wire.PeerID) wss.DialOptions {
	opts := p.Options
	if pin, ok := p.CertPins[id]; ok {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if opts.TLSConfig != nil {
			cfg = opts.TLSConfig.Clone()
		}
		previous := cfg.VerifyConnection
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || sha256.Sum256(cs.PeerCertificates[0].Raw) != pin {
				return errors.New("wss: relay certificate pin mismatch")
			}
			if previous != nil {
				return previous(cs)
			}
			return nil
		}
		opts.TLSConfig = cfg
	}
	return opts
}

func applyWSSConfig(out *Config, in repo.SwarmConfig) error {
	if in.Proxy != "" {
		p, err := url.Parse(in.Proxy)
		if err != nil || p.Scheme != "http" || p.Hostname() == "" || p.Path != "" && p.Path != "/" || p.RawQuery != "" || p.Fragment != "" {
			return errors.New("swarm.proxy: explicit http:// CONNECT proxy required")
		}
		out.WSS.Options.Proxy = p
	}
	if len(in.RelayCertPins) > 16 {
		return errors.New("swarm.relay_cert_pins: at most 16 pins")
	}
	out.WSS.CertPins = make(map[wire.PeerID][32]byte)
	for _, text := range in.RelayCertPins {
		peer, digest, ok := strings.Cut(text, "@")
		id, err := wire.ParsePeerID(peer)
		pin, pinErr := hex.DecodeString(digest)
		if !ok || err != nil || pinErr != nil || len(pin) != 32 {
			return errors.New("swarm.relay_cert_pins: peerID@hex-SHA256(leaf DER) required")
		}
		if _, exists := out.WSS.CertPins[id]; exists {
			return errors.New("swarm.relay_cert_pins: duplicate peer")
		}
		admitted := false
		for _, addr := range out.Relays {
			_, relay, err := wss.Endpoint(addr)
			if err == nil && relay == id {
				admitted = true
			}
		}
		if !admitted {
			return errors.New("swarm.relay_cert_pins: pin requires a configured WSS relay")
		}
		out.WSS.CertPins[id] = [32]byte(pin)
	}
	return nil
}

// addRelayLocked is used only by explicit configuration/link admission.
func (c *connectivity) addRelayLocked(addr wire.Multiaddr, limit int) error {
	id, err := circuit.RelayPeer(addr)
	if err != nil || id == c.s.ID() || c.s.host.IsLocalOnlyMultiaddr(addr) {
		return errors.New("swarm: relay must be a physical QUIC or WSS endpoint with another PeerID")
	}
	endpoints, exists := c.known[id]
	if !exists && len(c.known) >= limit {
		return host.ErrLimit
	}
	if err := endpoints.Add(addr); err != nil {
		return fmt.Errorf("swarm: %w", err)
	}
	c.known[id] = endpoints
	return nil
}

// Preserve the short QUIC-only failover probe; only a declared WSS alternative
// needs the larger independent outer setup budgets.
func (c *connectivity) recoveryBudget(addr wire.Multiaddr) time.Duration {
	_, id, _, err := circuit.ParseAddress(addr)
	if err != nil {
		return time.Second
	}
	c.mu.Lock()
	endpoints := c.known[id]
	c.mu.Unlock()
	if len(endpoints.WSS.Bytes()) != 0 {
		return 6 * time.Second
	}
	return time.Second
}

func (c *connectivity) dialRelay(ctx context.Context, addr wire.Multiaddr) (*host.Conn, error) {
	id, err := circuit.RelayPeer(addr)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	endpoints := c.known[id]
	c.mu.Unlock()
	if !endpoints.Contains(addr) {
		return nil, errors.New("swarm: relay endpoint not authorized")
	}
	conn, _, _, err := endpoints.Dial(ctx, c.s.host, 2*time.Second, c.s.cfg.WSS.options(id))
	return conn, err
}
