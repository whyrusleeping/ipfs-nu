// Package op is the in-memory operation manager behind the API's operation resource.
// Long-running work (add, fetch, pin, follow sync, ...) starts an Operation, reports stages
// and progress on it and finishes it; the API lists, streams and waits on them. The
// manager is process-local: operations do not survive a daemon restart, which is why list
// cursors carry the manager's epoch and answer resync after one.
package op

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
)

// Manager holds operations. The zero value is not usable; use NewManager.
type Manager struct {
	epoch string
	mu    sync.Mutex
	ops   map[string]*Operation
	order []*Operation // creation order

	// Retention of finished operations, so `nu status`/`operations` can still show them.
	keepFinished int
	keepFor      time.Duration

	shutdown atomic.Bool // set by CancelAll
}

// NewManager returns an empty manager with a fresh epoch.
func NewManager() *Manager {
	return &Manager{
		epoch:        newID(),
		ops:          map[string]*Operation{},
		keepFinished: 1000,
		keepFor:      time.Hour,
	}
}

// Epoch identifies this manager instance; cursors embed it.
func (m *Manager) Epoch() string { return m.epoch }

// Start registers a running operation of the given kind on an exact target. The returned
// operation's Context is cancelled by Cancel; work should watch it.
func (m *Manager) Start(kind, target string) *Operation {
	o, _ := m.StartWithID(newID(), kind, target)
	return o
}

// StartWithID creates an operation with a caller-persisted ID. It is for operation
// recovery across daemon restarts; callers must validate IDs before using them in paths.
func (m *Manager) StartWithID(id, kind, target string) (*Operation, error) {
	if id == "" {
		return nil, errors.New("operation: empty ID")
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()
	o := &Operation{
		m:      m,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		snap: apiv0.Operation{
			ID:      id,
			Kind:    kind,
			Target:  target,
			State:   apiv0.StateRunning,
			Created: now,
			Updated: now,
		},
	}
	m.mu.Lock()
	m.prune(now)
	if _, exists := m.ops[id]; exists {
		m.mu.Unlock()
		cancel()
		return nil, errors.New("operation: duplicate ID")
	}
	m.ops[o.snap.ID] = o
	m.order = append(m.order, o)
	m.mu.Unlock()
	return o, nil
}

// prune drops finished operations beyond the retention limits. Caller holds m.mu.
func (m *Manager) prune(now time.Time) {
	finished := 0
	for _, o := range m.order {
		if o.Snapshot().Done() {
			finished++
		}
	}
	kept := m.order[:0]
	for _, o := range m.order {
		s := o.Snapshot()
		if s.Done() && (finished > m.keepFinished || now.Sub(s.Updated) > m.keepFor) {
			finished--
			delete(m.ops, s.ID)
			continue
		}
		kept = append(kept, o)
	}
	m.order = kept
}

// Get returns an operation by ID.
func (m *Manager) Get(id string) (*Operation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.ops[id]
	return o, ok
}

// List returns snapshots of all retained operations, oldest first.
func (m *Manager) List() []apiv0.Operation {
	m.mu.Lock()
	ops := append([]*Operation(nil), m.order...)
	m.mu.Unlock()
	out := make([]apiv0.Operation, 0, len(ops))
	for _, o := range ops {
		out = append(out, o.Snapshot())
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Counts returns the number of operations per state.
func (m *Manager) Counts() map[apiv0.State]int {
	counts := map[apiv0.State]int{}
	for _, o := range m.List() {
		counts[o.State]++
	}
	return counts
}

// CancelAll cancels every running operation (daemon shutdown).
func (m *Manager) CancelAll() {
	m.shutdown.Store(true)
	m.mu.Lock()
	ops := append([]*Operation(nil), m.order...)
	m.mu.Unlock()
	for _, o := range ops {
		o.Cancel()
	}
}

// ShuttingDown reports whether CancelAll was called: durable work cancelled by it should
// keep its persisted state for recovery instead of recording a cancellation.
func (m *Manager) ShuttingDown() bool { return m.shutdown.Load() }

// Operation is one unit of long-running work.
type Operation struct {
	m      *Manager
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	snap   apiv0.Operation
	events []apiv0.Event // complete history; progress is expected to be rate-limited by callers
	subs   map[*subscriber]struct{}
}

// ID returns the operation ID.
func (o *Operation) ID() string { return o.snap.ID }

// Context is cancelled when the operation is cancelled; work must stop and call Fail or
// let Finish record the cancellation.
func (o *Operation) Context() context.Context { return o.ctx }

// Done is closed when the operation reaches a terminal state.
func (o *Operation) Done() <-chan struct{} { return o.done }

// Snapshot returns a copy of the current state.
func (o *Operation) Snapshot() apiv0.Operation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.snap
}

// Stage records a new stage name.
func (o *Operation) Stage(stage string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.snap.Done() {
		return
	}
	o.snap.Stage = stage
	o.emitLocked(apiv0.Event{Type: apiv0.EventStage, Stage: stage})
}

// Progress updates the counters. Callers should rate-limit (a few events per second).
func (o *Operation) Progress(done, total int64, unit string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.snap.Done() {
		return
	}
	o.snap.Progress = apiv0.Progress{Done: done, Total: total, Unit: unit}
	p := o.snap.Progress
	o.emitLocked(apiv0.Event{Type: apiv0.EventProgress, Progress: &p})
}

// Log records a human-readable line.
func (o *Operation) Log(msg string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.snap.Done() {
		return
	}
	o.emitLocked(apiv0.Event{Type: apiv0.EventLog, Message: msg})
}

// Succeed finishes the operation with a JSON-encodable result.
func (o *Operation) Succeed(result any) {
	var raw json.RawMessage
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			o.Fail(err)
			return
		}
		raw = b
	}
	o.finish(apiv0.StateSucceeded, raw, nil)
}

// Fail finishes the operation with an error. A context cancellation records the cancelled
// state; any other error becomes an apiv0.Error (kept as-is if it already is one).
func (o *Operation) Fail(err error) {
	var ae *apiv0.Error
	switch {
	case errors.As(err, &ae):
	case errors.Is(err, context.Canceled):
		o.finish(apiv0.StateCancelled, nil, &apiv0.Error{Code: apiv0.CodeCancelled, Message: "operation cancelled"})
		return
	default:
		ae = &apiv0.Error{Code: apiv0.CodeFailed, Message: err.Error()}
	}
	o.finish(apiv0.StateFailed, nil, ae)
}

// Finish ends the operation from a (result, err) pair: Succeed on nil error, else Fail.
func (o *Operation) Finish(result any, err error) {
	if err != nil {
		o.Fail(err)
		return
	}
	o.Succeed(result)
}

// Cancel cancels the operation's context. Work that stops should call Fail(ctx.Err()); if
// the operation is already finished this is a no-op.
func (o *Operation) Cancel() {
	o.cancel()
}

func (o *Operation) finish(state apiv0.State, result json.RawMessage, err *apiv0.Error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.snap.Done() {
		return
	}
	o.snap.State = state
	o.snap.Result = result
	o.snap.Error = err
	snap := o.snap
	o.emitLocked(apiv0.Event{Type: apiv0.EventDone, Operation: &snap})
	o.cancel()
	close(o.done)
}

func (o *Operation) emitLocked(ev apiv0.Event) {
	ev.Seq = int64(len(o.events)) + 1
	ev.Time = time.Now()
	o.snap.Updated = ev.Time
	o.events = append(o.events, ev)
	for s := range o.subs {
		s.notify()
	}
}

// Events returns a stream of this operation's events starting after seq `since` (0 for
// all). The stream replays history, then follows live events, and ends after the done
// event. Close the stream when finished with it.
func (o *Operation) Events(since int64) *Stream {
	s := &subscriber{op: o, next: since, wake: make(chan struct{}, 1)}
	o.mu.Lock()
	if o.subs == nil {
		o.subs = map[*subscriber]struct{}{}
	}
	o.subs[s] = struct{}{}
	o.mu.Unlock()
	return &Stream{s: s}
}

type subscriber struct {
	op   *Operation
	next int64 // seq of the last delivered event
	wake chan struct{}
}

func (s *subscriber) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Stream delivers an operation's events in order.
type Stream struct {
	s     *subscriber
	ended bool
}

// Next blocks for the next event. It returns io.EOF after the done event has been
// delivered, or ctx.Err() when ctx ends first.
func (st *Stream) Next(ctx context.Context) (*apiv0.Event, error) {
	if st.ended {
		return nil, io.EOF
	}
	s := st.s
	for {
		s.op.mu.Lock()
		if int(s.next) < len(s.op.events) {
			ev := s.op.events[s.next]
			s.next++
			s.op.mu.Unlock()
			if ev.Type == apiv0.EventDone {
				st.ended = true
			}
			return &ev, nil
		}
		s.op.mu.Unlock()
		select {
		case <-s.wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Close unsubscribes.
func (st *Stream) Close() error {
	o := st.s.op
	o.mu.Lock()
	delete(o.subs, st.s)
	o.mu.Unlock()
	return nil
}

// Wait blocks until the operation finishes or the timeout elapses (0 means no timeout).
// It returns the final snapshot, or the current one with a timeout error.
func (o *Operation) Wait(ctx context.Context, timeout time.Duration) (apiv0.Operation, error) {
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-o.done:
		return o.Snapshot(), nil
	case <-timer:
		snap := o.Snapshot()
		return snap, &apiv0.Error{
			Code:    apiv0.CodeTimeout,
			Message: "operation " + snap.ID + " is still " + string(snap.State) + " after " + timeout.String(),
			Action:  "wait longer, or follow its events",
			Details: map[string]any{"operation": snap},
		}
	case <-ctx.Done():
		return o.Snapshot(), ctx.Err()
	}
}

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newID returns a 16-byte random identifier in lowercase base32 (26 characters).
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	s := idEncoding.EncodeToString(b[:])
	out := make([]byte, len(s))
	for i := range s {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}
