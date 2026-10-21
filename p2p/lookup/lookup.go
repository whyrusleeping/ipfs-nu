// Package lookup combines bounded peer address sources without dialing or
// treating address claims as authorization.
package lookup

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

// Source must return when ctx is canceled and stop emitting when emit is false.
// Sources are queried concurrently; a slow source never delays a faster source.
type Source interface {
	Lookup(context.Context, wire.PeerID, func(Address) bool) error
}
type SourceFunc func(context.Context, wire.PeerID, func(Address) bool) error

func (f SourceFunc) Lookup(ctx context.Context, id wire.PeerID, emit func(Address) bool) error {
	return f(ctx, id, emit)
}

type Result struct {
	Address Address
	Err     error
}
type Resolver interface {
	Lookup(context.Context, wire.PeerID) <-chan Result
}
type Config struct {
	Cache       *Cache
	Sources     []Source
	MDNS        MDNSConfig
	DisableMDNS bool
	MaxLookups  int
	Timeout     time.Duration
}
type Service struct {
	cache   *Cache
	sources []Source
	mdns    *MDNS
	slots   chan struct{}
	timeout time.Duration
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	mu      sync.Mutex
	closed  bool
}

func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.MaxLookups == 0 {
		cfg.MaxLookups = 32
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 3 * time.Second
	}
	if cfg.MaxLookups < 0 || cfg.Timeout < 0 || len(cfg.Sources) > 8 {
		return nil, wire.ErrLimit
	}
	if cfg.Cache == nil {
		var err error
		cfg.Cache, err = NewCache(ctx, CacheConfig{})
		if err != nil {
			return nil, err
		}
	}
	child, cancel := context.WithCancel(ctx)
	s := &Service{cache: cfg.Cache, slots: make(chan struct{}, cfg.MaxLookups), timeout: cfg.Timeout, ctx: child, cancel: cancel}
	s.sources = append([]Source{s.cache}, cfg.Sources...)
	if !cfg.DisableMDNS {
		m, err := NewMDNS(child, cfg.MDNS, s.cache)
		if err != nil {
			cancel()
			return nil, err
		}
		s.mdns = m
		s.sources = append(s.sources, m)
	}
	return s, nil
}
func (s *Service) Cache() *Cache { return s.cache }
func (s *Service) RememberHints(ctx context.Context, hints []Hint) error {
	for _, h := range hints {
		if err := s.cache.Remember(ctx, h, "hint", 0.6, 24*time.Hour); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) Lookup(ctx context.Context, id wire.PeerID) <-chan Result {
	out := make(chan Result, 32)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		out <- Result{Err: context.Canceled}
		close(out)
		return out
	}
	select {
	case s.slots <- struct{}{}:
	default:
		s.mu.Unlock()
		out <- Result{Err: wire.ErrLimit}
		close(out)
		return out
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer func() { <-s.slots; close(out) }()
		child, cancel := context.WithTimeout(ctx, s.timeout)
		defer cancel()
		stop := context.AfterFunc(s.ctx, cancel)
		defer stop()
		incoming := make(chan Result, 32)
		var workers sync.WaitGroup
		for i, source := range s.sources {
			workers.Add(1)
			go func() {
				defer workers.Done()
				send := func(r Result) bool {
					select {
					case incoming <- r:
						return true
					case <-child.Done():
						return false
					}
				}
				err := source.Lookup(child, id, func(a Address) bool {
					if a.Peer != id {
						return send(Result{Err: ErrPeerMismatch})
					}
					normalized, err := NormalizeAddress(id, a.Addr)
					if err != nil {
						return send(Result{Err: err})
					}
					a.Addr = normalized
					return send(Result{Address: a})
				})
				if err != nil && child.Err() == nil {
					send(Result{Err: fmt.Errorf("lookup source %d: %w", i, err)})
				}
			}()
		}
		go func() { workers.Wait(); close(incoming) }()
		seen := make(map[string]bool)
		for r := range incoming {
			if r.Err == nil {
				key := string(r.Address.Addr.Bytes())
				if seen[key] {
					continue
				}
				if len(seen) >= 256 {
					cancel()
					continue
				}
				seen[key] = true
			}
			select {
			case out <- r:
			case <-child.Done():
			}
		}
	}()
	return out
}
func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	var err error
	if s.mdns != nil {
		err = s.mdns.Close()
	}
	s.wg.Wait()
	return err
}
