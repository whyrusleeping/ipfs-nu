package swarm

import (
	"context"
	"errors"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// reconnectInner checks the OLD authenticated session once after replacing a raw
// packet leg. STOP metadata is relay-asserted, not an authenticated restart signal.
// A ping exercises the old QUIC CID: the restarted endpoint can return a standard
// authenticated stateless reset. A timeout only invalidates this connection; a
// replacement must complete the ordinary peer-bound TLS handshake.
func (c *connectivity) reconnectInner(ctx context.Context, id wire.PeerID) (*host.Conn, error) {
	if old, ok := c.s.host.Connection(id); ok && c.s.host.IsLocalOnlyAddress(old.RemoteAddr()) {
		probe, cancel := context.WithTimeout(ctx, time.Second)
		_, err := c.s.identify.Ping(probe, id)
		cancel()
		if err != nil {
			if errors.Is(err, host.ErrLimit) || errors.Is(err, wire.ErrNotSupported) {
				return nil, err
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			_ = old.Close()
		}
	}
	return c.inner(ctx, id)
}
