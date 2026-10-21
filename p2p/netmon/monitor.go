// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause
// Adapted from tailscale.com v1.104.2 net/netmon/{netmon,polling}.go; see NOTICE.
// Nu retains bounded event coalescing and wall-clock wake detection, without
// Tailscale interface filters, eventbus, policy routing or control-plane state.
package netmon

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Kind uint32

const (
	Interface Kind = 1 << iota
	Address
	Route
	Sleep
	Wake
	Resync // full state must be re-read (startup, overflow or polling failure)
)

// Event is an invalidation, not a complete OS snapshot. Re-read interface/address/
// route state in hooks. Coalescing ORs kinds and preserves the earliest timestamp.
type Event struct {
	Kind Kind
	At   time.Time
	Gap  time.Duration
	Err  error
}
type Options struct {
	PollInterval  time.Duration // fallback polling and wall clock sampling; default 250ms
	WakeThreshold time.Duration // wall-time gap indicating sleep or clock jump; default 2s
}
type watcher func(context.Context, func(Event))

type Monitor struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
	next   uint64
	subs   map[uint64]chan Event
}

// Start opens the OS watcher before returning, so subsequent changes are observed.
// Linux uses rtnetlink; Darwin polls interface/address and route snapshots.
func Start(ctx context.Context, opts Options) (*Monitor, error) {
	if opts.PollInterval == 0 {
		opts.PollInterval = 250 * time.Millisecond
	}
	if opts.WakeThreshold == 0 {
		opts.WakeThreshold = 2 * time.Second
	}
	if opts.PollInterval <= 0 || opts.WakeThreshold <= opts.PollInterval {
		return nil, errors.New("netmon: invalid sampling intervals")
	}
	watch, err := openWatcher(opts.PollInterval)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	m := &Monitor{cancel: cancel, subs: make(map[uint64]chan Event)}
	m.wg.Add(2)
	go func() { defer m.wg.Done(); watch(ctx, m.emit) }()
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(opts.PollInterval)
		defer ticker.Stop()
		last := time.Now().Round(0)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now().Round(0)
				if e, ok := wallEvent(last, now, opts.WakeThreshold); ok {
					m.emit(e)
				}
				last = now
			}
		}
	}()
	return m, nil
}
func wallEvent(last, now time.Time, threshold time.Duration) (Event, bool) {
	gap := now.Sub(last)
	// This is a wake/clock-jump inference, not a pre-suspend OS notification.
	if gap > threshold || gap < 0 {
		return Event{Kind: Sleep | Wake | Resync, At: now, Gap: gap}, true
	}
	return Event{}, false
}

// Notify accepts explicit suspend/resume events from a platform integration, or
// a forced rescan. It is safe concurrently with the OS watcher.
func (m *Monitor) Notify(kind Kind) { m.emit(Event{Kind: kind, At: time.Now()}) }
func merge(a, b Event) Event {
	b.Kind |= a.Kind
	if a.At.Before(b.At) {
		b.At = a.At
	}
	if a.Gap > b.Gap {
		b.Gap = a.Gap
	}
	// Keep one diagnostic, not an unbounded error tree for a stalled subscriber.
	if b.Err == nil {
		b.Err = a.Err
	}
	return b
}
func (m *Monitor) emit(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	for _, ch := range m.subs {
		next := e
		select {
		case old := <-ch:
			next = merge(old, next)
		default:
		}
		select {
		case ch <- next:
		default:
		}
	}
}

// Subscribe returns a one-element coalescing queue and an idempotent unsubscribe.
// Its initial Resync event requests initial state; Close also closes subscriptions.
func (m *Monitor) Subscribe() (<-chan Event, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan Event, 1)
	if m.closed {
		close(ch)
		return ch, func() {}
	}
	m.next++
	id := m.next
	m.subs[id] = ch
	ch <- Event{Kind: Resync, At: time.Now()}
	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, ok := m.subs[id]; ok {
			delete(m.subs, id)
			close(ch)
		}
	}
}
func (m *Monitor) Close() error {
	m.cancel()
	m.wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		for id, ch := range m.subs {
			close(ch)
			delete(m.subs, id)
		}
	}
	return nil
}
