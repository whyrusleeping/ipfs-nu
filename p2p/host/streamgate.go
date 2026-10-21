package host

import (
	"fmt"

	"github.com/whyrusleeping/nu/p2p/wire"
)

// StreamGate decides whether an authenticated peer may open an inbound stream for
// protocol; a denied protocol is refused in negotiation like an unsupported one. It runs
// under the host lock, so it must be fast and must not call back into the host.
type StreamGate func(peer wire.PeerID, protocol string) bool

// SetStreamGate installs (or with nil removes) the inbound stream policy. Cluster gating
// uses it to admit a non-member joiner only to the join exchange.
func (h *Host) SetStreamGate(g StreamGate) {
	h.mu.Lock()
	h.streamGate = g
	h.mu.Unlock()
}

// SetDialGater installs (or with nil removes) an outbound-only policy checked before any
// packet is sent, in addition to the connection gaters: a private node may accept a
// joiner it would never dial.
func (h *Host) SetDialGater(g ConnectionGater) {
	h.mu.Lock()
	h.dialGate = g
	h.mu.Unlock()
}

func (h *Host) checkDial(id wire.PeerID) error {
	h.mu.Lock()
	g := h.dialGate
	h.mu.Unlock()
	if g != nil && !g(id) {
		return fmt.Errorf("%w: %s (dial)", ErrPeerDenied, id)
	}
	return nil
}
