package homerelay

import (
	"context"
	"errors"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Endpoints explicitly authorizes one QUIC endpoint for a relay.
// Never add addresses from remote records to this set.
type Endpoints struct{ QUIC wire.Multiaddr }

func (e *Endpoints) Add(addr wire.Multiaddr) error {
	if _, err := circuit.RelayPeer(addr); err != nil {
		return err
	}
	if len(e.QUIC.Bytes()) != 0 && e.QUIC.String() != addr.String() {
		return errors.New("relay endpoints: at most one QUIC endpoint per relay")
	}
	e.QUIC = addr
	return nil
}

func (e Endpoints) Contains(addr wire.Multiaddr) bool {
	return len(addr.Bytes()) != 0 && addr.String() == e.QUIC.String()
}

// Dial reuses a live authenticated connection or dials the admitted QUIC endpoint.
func (e Endpoints) Dial(ctx context.Context, h *host.Host, timeout time.Duration) (*host.Conn, wire.Multiaddr, error) {
	id, err := circuit.RelayPeer(e.QUIC)
	if err != nil {
		return nil, wire.Multiaddr{}, err
	}
	if c, ok := h.Connection(id); ok {
		return c, e.QUIC, nil
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	attempt, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := h.Dial(attempt, e.QUIC)
	if err != nil {
		return nil, wire.Multiaddr{}, err
	}
	return conn, e.QUIC, nil
}
