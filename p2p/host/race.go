package host

import (
	"context"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	ma "github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// AddressRank gives LAN/loopback first, then IPv6, then public IPv4, then
// unsupported DNS/circuit hints. These are ordering hints, never authorization.
func AddressRank(a wire.Multiaddr) int {
	if a.Opaque() {
		return 4
	}
	m, err := ma.NewMultiaddrBytes(a.Bytes())
	if err != nil {
		return 4
	}
	parts := ma.Split(m)
	if len(parts) == 0 {
		return 4
	}
	for _, part := range parts {
		if part.Protocol().Code == ma.P_CIRCUIT {
			return 4
		}
	}
	code := parts[0].Protocol().Code
	if code != ma.P_IP4 && code != ma.P_IP6 {
		return 3
	}
	ip := net.ParseIP(parts[0].Value())
	if ip == nil {
		return 4
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return 0
	}
	if code == ma.P_IP6 {
		return 1
	}
	return 2
}

type dialResult struct {
	address wire.Multiaddr
	conn    *Conn
	err     error
}

func (s *Session) race(ctx context.Context) (winner *Conn, err error) {
	m := s.owner
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	parallel := min(m.config.MaxParallelDials, m.config.MaxDials)
	results := make(chan dialResult, parallel)
	// Cancellation stops losing dials. Drain all admitted workers before returning
	// so no orphan successful connection or dialing goroutine survives an attempt.
	defer func() {
		cancel()
		workers.Wait()
		close(results)
		// Simultaneous inbound/outbound dials may have selected a different
		// canonical connection through the host's deterministic tie-break.
		original := winner
		if err == nil && winner != nil {
			if current, ok := m.host.Connection(s.peer); ok {
				winner = current
			} else {
				winner = nil
				err = ErrNoConnection
			}
		}
		if original != nil && original != winner {
			_ = original.Close()
		}
		for r := range results {
			if r.conn != nil && r.conn != winner {
				_ = r.conn.Close()
			}
			if err != nil {
				if r.err == nil {
					r.err = child.Err()
				}
				failureErr, ok := err.(*DialError)
				if ok {
					failureErr.Failures = append(failureErr.Failures, DialFailure{r.address, r.err})
				}
			}
		}
	}()
	failure := &DialError{Peer: s.peer}
	m.mu.Lock()
	explicit := append([]wire.Multiaddr(nil), m.peers[s.peer]...)
	m.mu.Unlock()
	var queue []lookup.Address
	seen := make(map[string]bool)
	add := func(a lookup.Address) {
		if len(seen)+len(failure.Failures) >= m.config.MaxAddresses {
			return
		}
		normalized, e := lookup.NormalizeAddress(s.peer, a.Addr)
		if e != nil {
			failure.Failures = append(failure.Failures, DialFailure{a.Addr, e})
			return
		}
		key := string(normalized.Bytes())
		if seen[key] {
			return
		}
		if len(seen) >= m.config.MaxAddresses {
			return
		}
		seen[key] = true
		a.Addr = normalized
		queue = append(queue, a)
		sort.SliceStable(queue, func(i, j int) bool {
			ri, rj := AddressRank(queue[i].Addr), AddressRank(queue[j].Addr)
			if ri != rj {
				return ri < rj
			}
			return queue[i].Confidence > queue[j].Confidence
		})
	}
	for _, a := range explicit {
		add(lookup.Address{Peer: s.peer, Addr: a, Source: "explicit", Confidence: 1})
	}
	if m.config.Cache != nil {
		for _, a := range m.config.Cache.Addresses(s.peer) {
			add(a)
		}
	}
	var discovery <-chan lookup.Result
	if !m.config.Private && m.config.Lookup != nil {
		discovery = m.config.Lookup.Lookup(child, s.peer)
	}
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	active := 0
	ready := true
	launch := func(a lookup.Address) {
		active++
		ready = false
		timer.Reset(m.config.Stagger)
		workers.Add(1)
		go func() {
			defer workers.Done()
			r := dialResult{address: a.Addr}
			select {
			case m.dials <- struct{}{}:
				dialctx, stop := context.WithTimeout(child, m.config.DialTimeout)
				r.conn, r.err = m.host.Dial(dialctx, a.Addr)
				stop()
				<-m.dials
			case <-child.Done():
				r.err = child.Err()
			}
			results <- r
		}()
	}
	for {
		// Drain immediately available cache/hint batches before choosing the next
		// address, without waiting for multicast or another slow source.
	drain:
		for drained := 0; discovery != nil && drained < m.config.MaxAddresses+16; drained++ {
			select {
			case r, ok := <-discovery:
				if !ok {
					discovery = nil
					break drain
				}
				if r.Err != nil {
					if len(failure.LookupErrors) < 16 {
						failure.LookupErrors = append(failure.LookupErrors, r.Err)
					}
				} else {
					add(r.Address)
				}
			default:
				break drain
			}
		}
		if len(queue) > 0 && active < parallel && (ready || active == 0) {
			a := queue[0]
			queue = queue[1:]
			launch(a)
		}
		if active == 0 && len(queue) == 0 && discovery == nil {
			return nil, failure
		}
		select {
		case r := <-results:
			active--
			if r.err == nil && r.conn != nil && r.conn.Context().Err() == nil {
				if ctx.Err() != nil {
					_ = r.conn.Close()
					return nil, ctx.Err()
				}
				if e := m.allowed(s.peer); e != nil {
					_ = r.conn.Close()
					return nil, e
				}
				winner = r.conn
				if m.config.Cache != nil {
					remote, _, _, parseErr := dialAddress(r.address)
					if parseErr == nil && remote.String() == winner.RemoteAddr().String() {
						if cacheErr := m.config.Cache.Remember(child, lookup.Hint{Peer: s.peer, Addresses: []wire.Multiaddr{r.address}}, "verified", 1, 7*24*time.Hour); cacheErr != nil {
							slog.Warn("peer address cache update failed", "peer", s.peer.String(), "err", cacheErr)
						}
					}
				}
				return winner, nil
			}
			if r.err == nil {
				r.err = net.ErrClosed
			}
			failure.Failures = append(failure.Failures, DialFailure{r.address, r.err})
		case r, ok := <-discovery:
			if !ok {
				discovery = nil
			} else if r.Err != nil {
				if len(failure.LookupErrors) < 16 {
					failure.LookupErrors = append(failure.LookupErrors, r.Err)
				}
			} else {
				add(r.Address)
			}
		case <-timer.C:
			ready = true
		case <-child.Done():
			for _, a := range queue {
				failure.Failures = append(failure.Failures, DialFailure{a.Addr, child.Err()})
			}
			failure.LookupErrors = append(failure.LookupErrors, child.Err())
			return nil, failure
		}
	}
}
