package host

import (
	"context"
	"sync"

	"github.com/whyrusleeping/nu/p2p/wire"
)

func (h *Host) Context() context.Context { return h.ctx }
func (h *Host) Connections() []*Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*Conn
	for _, list := range h.peers {
		for _, c := range list {
			if c.Context().Err() == nil {
				out = append(out, c)
			}
		}
	}
	return out
}

// WatchChanges returns a coalescing notification for connections/protocols.
// It is initially ready; readers re-snapshot Connections and Protocols after
// each notification. Cancel unregisters it; the channel is never closed.
func (h *Host) WatchChanges() (<-chan struct{}, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.watchers == nil {
		h.watchers = make(map[uint64]chan struct{})
	}
	h.nextWatcher++
	id := h.nextWatcher
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	h.watchers[id] = ch
	var once sync.Once
	return ch, func() { once.Do(func() { h.mu.Lock(); delete(h.watchers, id); h.mu.Unlock() }) }
}
func (h *Host) signalChangedLocked() {
	for _, ch := range h.watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// SealPeerRecord signs only records belonging to this host, without exposing
// its private node key to protocol services.
func (h *Host) SealPeerRecord(seq uint64, addrs []wire.Multiaddr, limits wire.RecordLimits) ([]byte, error) {
	return (wire.PeerRecord{PeerID: h.ID(), Seq: seq, Addresses: addrs}).Seal(h.key, limits)
}
