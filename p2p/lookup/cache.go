package lookup

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

// Entry is a persistence DTO. Confidence is its original confidence at Seen;
// consumers receive an effective, decayed confidence through Address.
type Entry struct {
	Peer       string    `json:"peer"`
	Address    string    `json:"address"`
	Source     string    `json:"source"`
	Seen       time.Time `json:"seen"`
	Expires    time.Time `json:"expires"`
	Confidence float64   `json:"confidence"`
}

// Persistence atomically replaces a bounded snapshot. SQLite can implement this
// boundary transactionally later. Implementations must honor context cancellation.
type Persistence interface {
	Load(context.Context) ([]Entry, error)
	Save(context.Context, []Entry) error
}
type Address struct {
	Peer          wire.PeerID
	Addr          wire.Multiaddr
	Source        string
	Seen, Expires time.Time
	Confidence    float64
}
type CacheConfig struct {
	Persistence                   Persistence
	Now                           func() time.Time
	HalfLife                      time.Duration
	MaxPeers, MaxAddressesPerPeer int
}
type Cache struct {
	mu      sync.Mutex
	config  CacheConfig
	entries []Entry
	changed chan struct{}
}

func NewCache(ctx context.Context, cfg CacheConfig) (*Cache, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HalfLife == 0 {
		cfg.HalfLife = 24 * time.Hour
	}
	if cfg.MaxPeers == 0 {
		cfg.MaxPeers = 256
	}
	if cfg.MaxAddressesPerPeer == 0 {
		cfg.MaxAddressesPerPeer = 32
	}
	if cfg.HalfLife < 0 || cfg.MaxPeers < 0 || cfg.MaxAddressesPerPeer < 0 {
		return nil, wire.ErrLimit
	}
	c := &Cache{config: cfg, changed: make(chan struct{})}
	if cfg.Persistence != nil {
		entries, err := cfg.Persistence.Load(ctx)
		if err != nil {
			return nil, err
		}
		if len(entries) > cfg.MaxPeers*cfg.MaxAddressesPerPeer {
			return nil, wire.ErrLimit
		}
		for _, e := range entries {
			if _, err := decodeEntry(e, cfg.Now()); err != nil {
				return nil, err
			}
		}
		c.entries = entries
		c.pruneLocked()
	}
	return c, nil
}
func decodeEntry(e Entry, now time.Time) (Address, error) {
	if len(e.Source) == 0 || len(e.Source) > 64 || math.IsNaN(e.Confidence) || math.IsInf(e.Confidence, 0) || e.Confidence < 0 || e.Confidence > 1 || e.Seen.IsZero() || e.Expires.Before(e.Seen) || e.Seen.After(now.Add(5*time.Minute)) {
		return Address{}, wire.ErrFormat
	}
	id, err := wire.ParsePeerID(e.Peer)
	if err != nil {
		return Address{}, err
	}
	a, err := wire.ParseMultiaddr(e.Address, 2048)
	if err != nil {
		return Address{}, err
	}
	a, err = NormalizeAddress(id, a)
	if err != nil {
		return Address{}, err
	}
	return Address{Peer: id, Addr: a, Source: e.Source, Seen: e.Seen, Expires: e.Expires, Confidence: e.Confidence}, nil
}
func (c *Cache) pruneLocked() {
	now := c.config.Now()
	live := c.entries[:0]
	for _, e := range c.entries {
		if e.Expires.After(now) {
			live = append(live, e)
		}
	}
	c.entries = live
	sort.SliceStable(c.entries, func(i, j int) bool { return c.entries[i].Seen.After(c.entries[j].Seen) })
	peers := make(map[string]int)
	live = c.entries[:0]
	for _, e := range c.entries {
		if peers[e.Peer] == 0 && len(peers) >= c.config.MaxPeers {
			continue
		}
		if peers[e.Peer] >= c.config.MaxAddressesPerPeer {
			continue
		}
		peers[e.Peer]++
		live = append(live, e)
	}
	c.entries = live
}
func (c *Cache) Remember(ctx context.Context, h Hint, source string, confidence float64, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(h.Addresses) > c.config.MaxAddressesPerPeer || ttl < 0 {
		return wire.ErrLimit
	}
	now := c.config.Now()
	next := append([]Entry(nil), c.entries...)
	for _, a := range h.Addresses {
		normalized, err := NormalizeAddress(h.Peer, a)
		if err != nil {
			return err
		}
		e := Entry{Peer: h.Peer.String(), Address: normalized.String(), Source: source, Seen: now, Expires: now.Add(ttl), Confidence: confidence}
		if _, err := decodeEntry(e, now); err != nil {
			return err
		}
		index := -1
		for i, old := range next {
			if old.Peer == e.Peer && old.Address == e.Address && old.Source == source {
				index = i
				break
			}
		}
		if index < 0 {
			next = append(next, e)
		} else {
			next[index] = e
		}
	}
	old := c.entries
	c.entries = next
	c.pruneLocked()
	if c.config.Persistence != nil {
		if err := c.config.Persistence.Save(ctx, c.entries); err != nil {
			c.entries = old
			return err
		}
	}
	close(c.changed)
	c.changed = make(chan struct{})
	return nil
}
func (c *Cache) Addresses(id wire.PeerID) []Address {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.addressesLocked(id)
}
func (c *Cache) addressesLocked(id wire.PeerID) []Address {
	now := c.config.Now()
	var out []Address
	for _, e := range c.entries {
		if e.Peer != id.String() || !e.Expires.After(now) {
			continue
		}
		a, err := decodeEntry(e, now)
		if err != nil {
			continue
		}
		age := max(0, now.Sub(e.Seen).Seconds())
		a.Confidence *= math.Exp2(-age / c.config.HalfLife.Seconds())
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Confidence > out[j].Confidence })
	return out
}
func (c *Cache) Lookup(ctx context.Context, id wire.PeerID, emit func(Address) bool) error {
	for _, a := range c.Addresses(id) {
		if !emit(a) {
			return ctx.Err()
		}
	}
	return nil
}
