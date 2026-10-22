package homerelay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/p2p/wss"
)

// Endpoints is an explicit authorization: at most one QUIC and one WSS endpoint
// for a single relay. Never add addresses from remote records to this set.
type Endpoints struct{ QUIC, WSS wire.Multiaddr }

func (e *Endpoints) Add(addr wire.Multiaddr) error {
	id, err := circuit.RelayPeer(addr)
	if err != nil {
		return err
	}
	for _, old := range []wire.Multiaddr{e.QUIC, e.WSS} {
		if len(old.Bytes()) == 0 {
			continue
		}
		peer, _ := circuit.RelayPeer(old)
		if peer != id {
			return errors.New("relay endpoints: different PeerIDs")
		}
	}
	slot := &e.QUIC
	if _, _, err := wss.Endpoint(addr); err == nil {
		slot = &e.WSS
	}
	if len(slot.Bytes()) != 0 && slot.String() != addr.String() {
		return errors.New("relay endpoints: at most one endpoint per transport")
	}
	*slot = addr
	return nil
}

func (e Endpoints) Contains(addr wire.Multiaddr) bool {
	return len(addr.Bytes()) != 0 && (addr.String() == e.QUIC.String() || addr.String() == e.WSS.String())
}

// Dial tries UDP first, then only the declared WSS alternative. Each attempt
// gets its own timeout; a UDP timeout cannot consume the WSS setup budget.
// The returned UDP error is evidence of a failed attempt, not proof of blocking.
func (e Endpoints) Dial(ctx context.Context, h *host.Host, timeout time.Duration, opts wss.DialOptions) (conn *host.Conn, selected wire.Multiaddr, udpError string, err error) {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	result := func(c *host.Conn, udp string) (*host.Conn, wire.Multiaddr, string, error) {
		addr := e.QUIC
		if c.Transport() == "wss" {
			addr = e.WSS
		}
		if len(addr.Bytes()) == 0 {
			return nil, addr, udp, errors.New("relay: active transport has no admitted endpoint")
		}
		return c, addr, udp, nil
	}
	// Reuse a live authenticated connection, reporting its actual transport.
	for _, addr := range []wire.Multiaddr{e.QUIC, e.WSS} {
		id, parseErr := circuit.RelayPeer(addr)
		if parseErr != nil {
			continue
		}
		if c, ok := h.Connection(id); ok {
			return result(c, "")
		}
	}
	if len(e.QUIC.Bytes()) != 0 {
		attempt, cancel := context.WithTimeout(ctx, timeout)
		conn, err = h.Dial(attempt, e.QUIC)
		cancel()
		if err == nil {
			return result(conn, "")
		}
		udpError = err.Error()
	}
	if len(e.WSS.Bytes()) != 0 && ctx.Err() == nil {
		attempt, cancel := context.WithTimeout(ctx, timeout)
		conn, err = h.DialWSS(attempt, e.WSS, opts)
		cancel()
		if err == nil {
			return result(conn, udpError)
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = errors.New("no admitted relay endpoint")
	}
	if udpError != "" {
		err = fmt.Errorf("relay UDP failed: %s; fallback: %w", udpError, err)
	}
	return nil, wire.Multiaddr{}, udpError, err
}
