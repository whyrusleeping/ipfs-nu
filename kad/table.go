package kad

import (
	"crypto/sha256"
	"math/bits"
	"sort"
	"sync"

	"github.com/whyrusleeping/nu/p2p/wire"
)

const K = 20
const Alpha = 3

type PeerInfo struct {
	ID    wire.PeerID
	Addrs [][]byte
}
type Table struct {
	mu      sync.RWMutex
	self    [32]byte
	buckets [256][]PeerInfo
}

func key(b []byte) [32]byte            { return sha256.Sum256(b) }
func NewTable(self wire.PeerID) *Table { return &Table{self: key(self.Bytes())} }
func bucket(a, b [32]byte) int {
	for i := range a {
		v := a[i] ^ b[i]
		if v != 0 {
			return i*8 + bits.LeadingZeros8(v)
		}
	}
	return -1
}

// Add inserts or refreshes a peer, retaining the most-recent K peers per bucket.
func (t *Table) Add(p PeerInfo) bool {
	if p.ID == "" {
		return false
	}
	id := key(p.ID.Bytes())
	i := bucket(t.self, id)
	if i < 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buckets[i]
	for j, x := range b {
		if x.ID == p.ID {
			b = append(b[:j], b[j+1:]...)
			t.buckets[i] = append(b, p)
			return true
		}
	}
	if len(b) >= K {
		copy(b, b[1:])
		b[len(b)-1] = p
	} else {
		b = append(b, p)
	}
	t.buckets[i] = b
	return true
}
func (t *Table) Remove(id wire.PeerID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	i := bucket(t.self, key(id.Bytes()))
	if i < 0 {
		return
	}
	b := t.buckets[i]
	for j, p := range b {
		if p.ID == id {
			t.buckets[i] = append(b[:j], b[j+1:]...)
			return
		}
	}
}
func (t *Table) Peers() []PeerInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []PeerInfo
	for _, b := range t.buckets {
		out = append(out, b...)
	}
	return out
}
func (t *Table) Closest(target []byte, n int) []PeerInfo {
	if n <= 0 {
		return nil
	}
	dst := key(target)
	peers := t.Peers()
	sort.Slice(peers, func(i, j int) bool { return closer(key(peers[i].ID.Bytes()), key(peers[j].ID.Bytes()), dst) })
	if len(peers) > n {
		peers = peers[:n]
	}
	return peers
}
func closer(a, b, d [32]byte) bool {
	for i := range a {
		if a[i]^d[i] != b[i]^d[i] {
			return a[i]^d[i] < b[i]^d[i]
		}
	}
	return false
}
