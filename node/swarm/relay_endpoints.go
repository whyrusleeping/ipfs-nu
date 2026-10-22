package swarm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// addRelayLocked is used only by explicit configuration/link admission.
func (c *connectivity) addRelayLocked(addr wire.Multiaddr, limit int) error {
	id, err := circuit.RelayPeer(addr)
	if err != nil || id == c.s.ID() || c.s.host.IsLocalOnlyMultiaddr(addr) {
		return errors.New("swarm: relay must be a physical QUIC endpoint with another PeerID")
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
	conn, _, err := endpoints.Dial(ctx, c.s.host, 2*time.Second)
	return conn, err
}
