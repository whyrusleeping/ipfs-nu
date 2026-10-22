package host

import (
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/whyrusleeping/nu/p2p/wire"
)

var ErrPeerDenied = errors.New("host: peer denied by connection policy")

// ConnectionGater must be fast and concurrency-safe. It runs outside host locks.
// Multiple policies intersect; addresses never grant identity authorization.
type ConnectionGater func(wire.PeerID) bool

func (h *Host) checkPeer(id wire.PeerID) error { _, err := h.checkPeerVersion(id); return err }
func (h *Host) checkPeerVersion(id wire.PeerID) (uint64, error) {
	h.mu.Lock()
	version := h.gateVersion
	primary := h.gater
	policies := make([]ConnectionGater, 0, len(h.gaters))
	for _, g := range h.gaters {
		policies = append(policies, g)
	}
	h.mu.Unlock()
	if primary != nil && !primary(id) {
		return version, fmt.Errorf("%w: %s", ErrPeerDenied, id)
	}
	for _, g := range policies {
		if !g(id) {
			return version, fmt.Errorf("%w: %s", ErrPeerDenied, id)
		}
	}
	return version, nil
}

// SetConnectionGater replaces the primary policy and disconnects newly denied
// peers. Additional guards (e.g. private sessions) cannot be overridden by it.
func (h *Host) SetConnectionGater(g ConnectionGater) {
	h.mu.Lock()
	h.gater = g
	h.gateVersion++
	h.mu.Unlock()
	h.RecheckConnections()
}
func (h *Host) AddConnectionGater(g ConnectionGater) (func(), error) {
	if g == nil {
		return nil, wire.ErrFormat
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, net.ErrClosed
	}
	if h.gaters == nil {
		h.gaters = make(map[uint64]ConnectionGater)
	}
	h.nextGater++
	id := h.nextGater
	h.gaters[id] = g
	h.gateVersion++
	h.mu.Unlock()
	h.RecheckConnections()
	var once sync.Once
	return func() { once.Do(func() { h.mu.Lock(); delete(h.gaters, id); h.gateVersion++; h.mu.Unlock() }) }, nil
}
func (h *Host) RecheckConnections() {
	h.mu.Lock()
	h.gateVersion++
	h.mu.Unlock()
	for _, c := range h.Connections() {
		if h.checkPeer(c.PeerID()) != nil {
			_ = c.Close()
		}
	}
}
func (h *Host) Disconnect(id wire.PeerID) {
	for _, c := range h.Connections() {
		if c.PeerID() == id {
			_ = c.Close()
		}
	}
}
