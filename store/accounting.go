package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
)

// Accounting and budgets (D17 A1–A8). Physical bytes per class are counters in the
// `usage` table, maintained by triggers in the same transactions that insert and delete
// rows, so admission is O(1); the database file, staging and free space are stat'ed.

// MaxBlobSize bounds the declared size of a partial blob (A6): 8 TiB keeps the verified
// group bitmap at 64 MiB. Larger claims are refused before any byte is fetched.
const MaxBlobSize = 8 << 40

// Limits are the store's budgets (A5). Zero values mean: no physical limit, no free-space
// reserve, watermark 0.9 and a one-hour GC grace.
type Limits struct {
	// Physical is store.limit: the physical byte budget (0: unlimited).
	Physical int64
	// FreeReserve is store.free_reserve: free bytes kept on the store's filesystem.
	FreeReserve int64
	// Watermark is the fraction of Physical above which GCIfNeeded collects.
	Watermark float64
	// Grace is GCOptions.Grace for automatic collections.
	Grace time.Duration
	// StateReserve is store.state_reserve: headroom only signed cluster-state writes
	// (Store.State) may use, capped at an eighth of Physical; content admission leaves it.
	StateReserve int64
}

// stateReserve is the effective state reserve.
func (l Limits) stateReserve() int64 {
	if l.Physical > 0 {
		return min(l.StateReserve, l.Physical/8)
	}
	return l.StateReserve
}

func (l Limits) withDefaults() Limits {
	if l.Watermark <= 0 || l.Watermark > 1 {
		l.Watermark = 0.9
	}
	if l.Grace == 0 {
		l.Grace = time.Hour
	}
	return l
}

// SetLimits replaces the budgets at runtime.
func (s *Store) SetLimits(l Limits) {
	s.limMu.Lock()
	s.limits = l.withDefaults()
	s.limMu.Unlock()
	s.resMu.Lock()
	s.admitted = admitCache{}
	s.resMu.Unlock()
}

// admitCache is the last accounting snapshot admission worked from.
type admitCache struct {
	at       time.Time
	headroom int64 // full headroom (state reserve included); -1 unlimited
	used     int64 // admitted since at
}

// Limits returns the current budgets.
func (s *Store) Limits() Limits {
	s.limMu.RLock()
	defer s.limMu.RUnlock()
	return s.limits
}

// pressureGCInterval rate-limits collections triggered by admission shortfalls (A7).
const pressureGCInterval = time.Minute

// Stats is the store's accounting for status output (A8). Bytes are physical: what the
// store occupies on disk, each object once. Free and Headroom are -1 when unknown or
// unlimited.
type Stats struct {
	// Objects counts blocks, blobs (complete) and partial blobs.
	Objects int64
	Pins    int64
	// Bytes is the physical total: Blobs + Outboards + Partials + DB + Staging.
	Bytes int64
	// Blocks is the logical size of the blocks in the database (part of DB).
	Blocks int64
	// Blobs is complete copied blob data; Outboards their Bao outboards (also of partials
	// and --nocopy references); Partials the verified bytes of partial blobs; DB the
	// database file with its WAL; Staging in-progress writes.
	Blobs, Outboards, Partials, DB, Staging int64
	// Reserved is admitted but not yet written: the missing bytes of open partial blobs.
	Reserved int64
	// NoCopy is the referenced bytes of --nocopy sources, not charged (A3).
	NoCopy int64
	// Free is the free space on the store's filesystem.
	Free int64
	// Limit and Reserve echo Limits.Physical and Limits.FreeReserve; StateReserve is the
	// effective state reserve.
	Limit, Reserve, StateReserve int64
	// Headroom is min(Limit − Bytes − Reserved, Free − Reserve) over the terms that apply,
	// less StateReserve, never below 0; -1 when neither applies. It is what content
	// admission may use.
	Headroom int64
	// full is Headroom with the state reserve (what signed state writes may use).
	full int64
	// Pressure is set while Headroom is 0.
	Pressure bool
}

// Stats returns the accounting.
func (s *Store) Stats() (Stats, error) {
	st, err := s.stats()
	if err != nil {
		return st, err
	}
	var blocks, blobs, parts, pins sql.NullInt64
	err = s.db.QueryRow(`SELECT (SELECT count(*) FROM blocks), (SELECT count(*) FROM blobs),
		(SELECT count(*) FROM partials), (SELECT count(*) FROM pins)`).Scan(&blocks, &blobs, &parts, &pins)
	st.Objects = blocks.Int64 + blobs.Int64 + parts.Int64
	st.Pins = pins.Int64
	return st, err
}

// stats is Stats without the counts.
func (s *Store) stats() (Stats, error) {
	var st Stats
	rows, err := s.db.Query(`SELECT class, bytes FROM usage`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var class string
		var n int64
		if err := rows.Scan(&class, &n); err != nil {
			return st, err
		}
		switch class {
		case "blocks":
			st.Blocks = n
		case "blobs":
			st.Blobs = n
		case "outboards":
			st.Outboards = n
		case "partials":
			st.Partials = n
		case "nocopy":
			st.NoCopy = n
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	for _, f := range []string{"nu.db", "nu.db-wal"} {
		if fi, err := os.Stat(filepath.Join(s.dir, f)); err == nil {
			st.DB += fi.Size()
		}
	}
	if ents, err := os.ReadDir(filepath.Join(s.dir, "staging")); err == nil {
		for _, e := range ents {
			if fi, err := e.Info(); err == nil {
				st.Staging += fi.Size()
			}
		}
	}
	s.resMu.Lock()
	st.Reserved = s.reserved
	s.resMu.Unlock()
	st.Bytes = st.Blobs + st.Outboards + st.Partials + st.DB + st.Staging
	st.Free = freeSpace(s.dir)
	l := s.Limits()
	st.Limit, st.Reserve = l.Physical, l.FreeReserve
	st.Headroom = -1
	if l.Physical > 0 {
		st.Headroom = max(l.Physical-st.Bytes-st.Reserved, 0)
	}
	if st.Free >= 0 && l.FreeReserve > 0 {
		h := max(st.Free-l.FreeReserve, 0)
		if st.Headroom < 0 || h < st.Headroom {
			st.Headroom = h
		}
	}
	st.full, st.StateReserve = st.Headroom, l.stateReserve()
	if st.Headroom >= 0 {
		st.Headroom = max(st.Headroom-st.StateReserve, 0)
	}
	st.Pressure = st.Headroom == 0
	return st, nil
}

// Usage returns the physical bytes the store holds (Stats.Bytes).
func (s *Store) Usage() (int64, error) {
	st, err := s.stats()
	return st.Bytes, err
}

// BudgetError is an admission refusal (A6): the write would exceed the physical limit
// (Reason "store_limit") or the free-space reserve ("free_space").
type BudgetError struct {
	Reason                                         string
	Need, Headroom, Physical, Limit, Free, Reserve int64
}

func (e *BudgetError) Error() string {
	switch e.Reason {
	case "free_space":
		return fmt.Sprintf("store: insufficient storage: need %d bytes, %d available above the %d-byte free-space reserve (free %d)",
			e.Need, e.Headroom, e.Reserve, e.Free)
	default:
		return fmt.Sprintf("store: insufficient storage: need %d bytes, %d left of the %d-byte limit (physical %d)",
			e.Need, e.Headroom, e.Limit, e.Physical)
	}
}

// admitCacheTTL is how long an admission reuses the last accounting snapshot, charging
// what it admitted meanwhile, so small writes do not pay a stat and a query each.
const admitCacheTTL = 250 * time.Millisecond

// admit checks that need more bytes of content fit the budgets, after one rate-limited
// reclaim when they do not. Callers hold no store lock.
func (s *Store) admit(need int64) error { return s.admitAs(need, false) }

// admitAs is admit; a state write may also use the state reserve.
func (s *Store) admitAs(need int64, state bool) error {
	avail := func(st Stats) int64 {
		if state {
			return st.full
		}
		return st.Headroom
	}
	keep := int64(0) // what this write must leave of the cached full headroom
	if !state {
		keep = s.Limits().stateReserve()
	}
	s.resMu.Lock()
	c := s.admitted
	if !c.at.IsZero() && s.now().Sub(c.at) < admitCacheTTL && (c.headroom < 0 || need <= c.headroom-c.used-keep) {
		s.admitted.used += need
		s.resMu.Unlock()
		return nil
	}
	s.resMu.Unlock()
	st, err := s.stats()
	if err != nil {
		return err
	}
	if avail(st) < 0 || need <= avail(st) {
		s.resMu.Lock()
		s.admitted = admitCache{at: s.now(), headroom: st.full, used: need}
		s.resMu.Unlock()
		return nil
	}
	if s.pressureGC() {
		if st, err = s.stats(); err != nil {
			return err
		}
		if avail(st) < 0 || need <= avail(st) {
			return nil
		}
	}
	e := &BudgetError{Reason: "store_limit", Need: need, Headroom: avail(st), Physical: st.Bytes + st.Reserved,
		Limit: st.Limit, Free: st.Free, Reserve: st.Reserve}
	if st.Free >= 0 && st.Reserve > 0 && (st.Limit == 0 || st.Free-st.Reserve < st.Limit-e.Physical) {
		e.Reason = "free_space"
	}
	return e
}

// pressureGC runs a collection for an admission shortfall unless one ran within
// pressureGCInterval; it reports whether it ran.
func (s *Store) pressureGC() bool {
	s.resMu.Lock()
	if s.now().Sub(s.lastPressureGC) < pressureGCInterval {
		s.resMu.Unlock()
		return false
	}
	s.lastPressureGC = s.now()
	s.resMu.Unlock()
	_, err := s.GC(context.Background(), GCOptions{Grace: s.Limits().Grace})
	return err == nil
}

// reserve records bytes admitted for an open partial blob; release returns them.
func (s *Store) reserve(n int64) {
	s.resMu.Lock()
	s.reserved += n
	s.resMu.Unlock()
}

// GCIfNeeded collects when the physical bytes reach the watermark of the limit or free
// space is below the reserve (A7), and reports whether it ran. It is the daemon's
// periodic maintenance call.
func (s *Store) GCIfNeeded(ctx context.Context) (bool, GCResult, error) {
	st, err := s.stats()
	if err != nil {
		return false, GCResult{}, err
	}
	l := s.Limits()
	over := l.Physical > 0 && float64(st.Bytes+st.Reserved) >= l.Watermark*float64(l.Physical)
	low := st.Free >= 0 && l.FreeReserve > 0 && st.Free < l.FreeReserve
	if !over && !low {
		return false, GCResult{}, nil
	}
	r, err := s.GC(ctx, GCOptions{Grace: l.Grace})
	return true, r, err
}

// ScopeUsage is the attribution of one scope (A4): a pin source plus name.
type ScopeUsage struct {
	Source, Name string
	Pins         int
	// Logical is the held logical bytes its pins reach, each object once per scope;
	// Missing counts reachable objects not held complete.
	Logical int64
	Missing int
	// Physical is the physical bytes of everything its pins reach; Unique the part no
	// other scope reaches, which releasing the scope would free after GC.
	Physical, Unique int64
}

// Attribution is one mark pass over every pin (A4).
type Attribution struct {
	Scopes []ScopeUsage
	// Shared is the physical bytes reachable from two or more scopes; Unpinned those
	// reachable from no pin (grace-protected adds and fetches, cache, abandoned partials).
	Shared, Unpinned int64
}

// Attribution walks every pin once and attributes physical bytes to scopes.
func (s *Store) Attribution(ctx context.Context) (Attribution, error) {
	pins, err := s.Pins()
	if err != nil {
		return Attribution{}, err
	}
	type mark struct {
		scope int32
		multi bool
		phys  int64
	}
	marks := map[cid.Multihash]*mark{}
	scopeIdx := map[[2]string]int{}
	var scopes []ScopeUsage
	for _, p := range pins {
		key := [2]string{p.Source, p.Name}
		si, ok := scopeIdx[key]
		if !ok {
			si = len(scopes)
			scopeIdx[key] = si
			scopes = append(scopes, ScopeUsage{Source: p.Source, Name: p.Name})
		}
		sc := &scopes[si]
		sc.Pins++
		seen := map[cid.Multihash]bool{} // once per scope
		_, err := s.walk(ctx, p, func(c cid.CID, info Info, held bool) error {
			m := c.Multihash()
			if seen[m] {
				return nil
			}
			seen[m] = true
			if held {
				sc.Logical += int64(info.Size)
			} else {
				sc.Missing++
			}
			sc.Physical += int64(info.Physical)
			if mk, ok := marks[m]; ok {
				if mk.scope != int32(si) {
					mk.multi = true
				}
			} else {
				marks[m] = &mark{scope: int32(si), phys: int64(info.Physical)}
			}
			return nil
		})
		if err != nil {
			return Attribution{}, err
		}
	}
	var a Attribution
	var pinned int64
	for _, mk := range marks {
		pinned += mk.phys
		if mk.multi {
			a.Shared += mk.phys
		} else {
			scopes[mk.scope].Unique += mk.phys
		}
	}
	st, err := s.stats()
	if err != nil {
		return a, err
	}
	a.Unpinned = max(st.Blobs+st.Outboards+st.Partials+st.Blocks-pinned, 0)
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].Source != scopes[j].Source {
			return scopes[i].Source < scopes[j].Source
		}
		return scopes[i].Name < scopes[j].Name
	})
	a.Scopes = scopes
	return a, nil
}

// heldBytes is the verified bytes of a partial blob: its held groups, the last one short.
func heldBytes(groups []byte, size uint64) int64 {
	n := blob.Groups(size)
	var held int64
	for g := uint64(0); g < n; g++ {
		if groups[g/8]&(1<<(g%8)) != 0 {
			_, bytes := (blob.Range{Start: g, End: g + 1}).Bytes(size)
			held += int64(bytes)
		}
	}
	return held
}

// Meter is an interval transfer budget (operations T1): bytes charged within a window of
// Interval starting at the first charge; the window resets when it has elapsed. It is the
// hook follow and cluster consumers configure per scope; the store itself charges nothing.
type Meter struct {
	mu       sync.Mutex
	limit    int64
	interval time.Duration
	now      func() time.Time
	start    time.Time
	used     int64
}

// MeterState is a Meter snapshot for status output.
type MeterState struct {
	Used, Limit int64
	Interval    time.Duration
	// ResetsAt is when the window ends (zero before the first charge).
	ResetsAt time.Time
}

// TransferError is a Meter refusal (T2): the charge would exceed the budget.
type TransferError struct {
	Need int64
	MeterState
}

func (e *TransferError) Error() string {
	return fmt.Sprintf("store: transfer budget: %d bytes would exceed %d of %d per %v (resets %s)",
		e.Need, e.Used, e.Limit, e.Interval, e.ResetsAt.Format(time.RFC3339))
}

// NewMeter returns a meter allowing limit bytes per interval; limit 0 is unlimited.
func NewMeter(limit int64, interval time.Duration) *Meter {
	return &Meter{limit: limit, interval: interval, now: time.Now}
}

func (m *Meter) roll() {
	t := m.now()
	if m.start.IsZero() || !t.Before(m.start.Add(m.interval)) {
		m.start, m.used = t, 0
	}
}

// Charge records n bytes, refusing (and recording nothing) when they would exceed the
// budget in the current window.
func (m *Meter) Charge(n int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roll()
	if m.limit > 0 && m.used+n > m.limit {
		return &TransferError{Need: n, MeterState: m.stateLocked()}
	}
	m.used += n
	return nil
}

// State returns the current window.
func (m *Meter) State() MeterState {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roll()
	return m.stateLocked()
}

func (m *Meter) stateLocked() MeterState {
	st := MeterState{Used: m.used, Limit: m.limit, Interval: m.interval}
	if !m.start.IsZero() {
		st.ResetsAt = m.start.Add(m.interval)
	}
	return st
}
