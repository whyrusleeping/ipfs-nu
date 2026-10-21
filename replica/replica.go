// Package replica runs cluster placement on a member (docs/cluster.md "Placement",
// "Sync, handoff and repair", "Status"): for every dataset the member is eligible for it
// pins the head, fetches its structure and the blobs it owns in the current placement
// epoch from members in placement order, checks completeness, and produces the
// completion reports heartbeats carry. It keeps the protected versions of
// docs/operations.md S5/S7 pinned and never releases anything (release.auto = false).
package replica

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// Defaults.
const (
	DefaultStagingMax = 2 // staging.max_heads (S5)
	DefaultInterval   = 30 * time.Second
	stateFile         = "replica.json"
)

// FetchFunc fetches root's structure and the blobs want selects, trying each object at the
// peers rank returns first, from members.
type FetchFunc func(ctx context.Context, root cid.CID, members []wire.PeerID, want func(cid.CID) bool, rank func(cid.CID) []wire.PeerID) error

// Config configures a Manager.
type Config struct {
	Store   *store.Store
	Cluster *cluster.Cluster
	Self    wire.PeerID
	// Dir holds the manager's state file (the cluster's directory).
	Dir string
	// Fetch brings content; nil when offline (only held content is used).
	Fetch FetchFunc
	// Ready reports whether target is policy-ready (E6) for dataset name as this node
	// evaluates it; nil: never (the node then keeps S5 first-held protection).
	Ready func(name string, target cid.CID) bool
	// Changed is called (without locks) when this node's reports change.
	Changed    func()
	Interval   time.Duration // 0: DefaultInterval
	StagingMax int           // 0: DefaultStagingMax
	Logger     *slog.Logger
}

// Protection is the persisted protection state of one dataset (S5).
type Protection struct {
	// Ready is the last head this node evaluated policy-ready; Held the last target it
	// fully held; Staged the incoming targets pinned for acquisition; Retained earlier
	// held/ready targets still pinned because release.auto is off (release candidates).
	Ready    string   `json:"ready,omitempty"`
	Held     string   `json:"held,omitempty"`
	Staged   []string `json:"staged,omitempty"`
	Retained []string `json:"retained,omitempty"`
}

// Status is this member's sync state of one dataset.
type Status struct {
	Name string
	// Target is what the member works toward (the head, or under S5 a staged head);
	// Observed is the head when it is only observed (staging full).
	Target, Observed cid.CID
	Mode             string // owned (eligible) or full (holder outside placement)
	Policy, Epoch    string
	State            string // syncing, held, paused, error
	Complete         bool
	Owned, Missing   cluster.Counts
	// StructureMissing counts structure nodes not held (the owner set is then unknown).
	StructureMissing int
	Reason, Detail   string
	Need             int64 // bytes needed when paused for storage (S7)
	Protection           Protection
	// Reported is the report this status yields.
	Reported cluster.Report
}

// Manager runs placement sync for one cluster.
type Manager struct {
	cfg  Config
	log  *slog.Logger
	kick chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	state    map[string]*Protection
	status   map[string]*Status
	inflight map[string]*fetching
	fetched  map[string]fetchOutcome
}

type fetching struct {
	target cid.CID
	cancel context.CancelFunc
}

// fetchOutcome is the result of the last finished fetch of a dataset.
type fetchOutcome struct {
	target cid.CID
	err    error
}

// Open loads the manager's state; Start runs it.
func Open(cfg Config) (*Manager, error) {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.StagingMax <= 0 {
		cfg.StagingMax = DefaultStagingMax
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	m := &Manager{cfg: cfg, log: cfg.Logger, kick: make(chan struct{}, 1), state: map[string]*Protection{}, status: map[string]*Status{},
		inflight: map[string]*fetching{}, fetched: map[string]fetchOutcome{}}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	b, err := os.ReadFile(filepath.Join(cfg.Dir, stateFile))
	if err == nil {
		if err := json.Unmarshal(b, &m.state); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	cfg.Cluster.OnChange(m.Kick)
	return m, nil
}

// Start runs the reconcile loop.
func (m *Manager) Start() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		tick := time.NewTicker(m.cfg.Interval)
		defer tick.Stop()
		for {
			m.Reconcile()
			select {
			case <-m.ctx.Done():
				return
			case <-m.kick:
			case <-tick.C:
			}
		}
	}()
}

// Close stops the loop and running fetches.
func (m *Manager) Close() error {
	m.cancel()
	m.wg.Wait()
	return nil
}

// Kick schedules a reconcile.
func (m *Manager) Kick() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *Manager) saveLocked() error {
	b, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(m.cfg.Dir, stateFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(m.cfg.Dir, stateFile))
}

// Placed reports whether a dataset receives placement (guaranteed copies): a non-collection
// dataset, or a collection, with replicas > 0 or all. Collection floor placement is not
// run here yet; collections are placed by the collection manager.
func Placed(d *cluster.Dataset) bool {
	return d != nil && d.Collection == nil && (d.Policy.Replicas > 0 || d.Policy.All)
}

func pinFor(name string, c cid.CID) store.Pin {
	return store.Pin{CID: c, Mode: store.Recursive, Profile: "nu-tree", Source: store.SourceCluster, Name: "dataset/" + name}
}

// Reconcile runs one pass over every dataset.
func (m *Manager) Reconcile() {
	v := m.cfg.Cluster.View()
	changed := false
	names := map[string]bool{}
	for name, sd := range v.Datasets {
		if sd.Deleted || !Placed(sd.Value) {
			continue
		}
		names[name] = true
		if m.reconcileOne(v, name, sd.Value) {
			changed = true
		}
	}
	m.mu.Lock()
	for name := range m.status {
		if !names[name] {
			delete(m.status, name)
			changed = true
		}
	}
	m.mu.Unlock()
	if changed && m.cfg.Changed != nil {
		m.cfg.Changed()
	}
}

func contains(s []string, x string) bool { return slices.Contains(s, x) }

// reconcileOne updates one dataset and reports whether its report changed.
func (m *Manager) reconcileOne(v *cluster.View, name string, d *cluster.Dataset) bool {
	head, ok := v.Head(name)
	var target cid.CID
	var size uint64
	if ok {
		target, _ = cid.Parse(head.Value.Target)
		size = uint64(max(head.Value.Size, 0))
	}
	pl := cluster.NewPlacement(v, d.Policy, m.cfg.Self)
	eligible := pl.Eligible(m.cfg.Self)

	m.mu.Lock()
	st := m.state[name]
	if st == nil {
		st = &Protection{}
		m.state[name] = st
	}
	prev := m.status[name]
	m.mu.Unlock()

	if !target.Defined() || (!eligible && st.Held == "" && len(st.Staged) == 0) {
		m.setStatus(name, nil)
		return prev != nil
	}
	s := &Status{Name: name, Policy: d.PolicyHash(), Epoch: pl.Epoch, Mode: cluster.ModeOwned}

	// Choose the work target (S5): with a policy-ready version the head is acquired
	// directly; without one, at most StagingMax incoming heads are staged and later heads
	// are only observed while staging is full.
	work := target
	ts := target.String()
	m.mu.Lock()
	switch {
	case !eligible:
		// A holder outside placement (draining, filtered out) keeps what it holds and
		// reports it in mode full; it acquires nothing.
		s.Mode = cluster.ModeFull
		if st.Held != ts && !contains(st.Staged, ts) {
			if c, err := cid.Parse(st.Held); err == nil {
				work = c
			} else if n := len(st.Staged); n > 0 {
				work, _ = cid.Parse(st.Staged[n-1])
			}
		}
	case ts == st.Held || contains(st.Staged, ts):
	case st.Ready != "":
		// Intermediate heads may be skipped: drop an abandoned staged head.
		m.abandonLocked(name, st, "")
		st.Staged = append(st.Staged, ts)
	case len(st.Staged) < m.cfg.StagingMax:
		st.Staged = append(st.Staged, ts)
	default:
		s.Observed = target
		c, _ := cid.Parse(st.Staged[len(st.Staged)-1])
		work = c
	}
	_ = m.saveLocked()
	m.mu.Unlock()
	s.Target = work
	if err := m.cfg.Store.Pin(pinFor(name, work)); err != nil {
		s.State, s.Reason, s.Detail = cluster.SyncError, "pin", err.Error()
		return m.finish(name, s, prev)
	}
	workSize := uint64(0)
	if work == target {
		workSize = size
	}

	chk := m.check(work, workSize, pl, s.Mode == cluster.ModeFull)
	s.Owned, s.Missing, s.StructureMissing, s.Complete = chk.owned, chk.missing, chk.structMissing, chk.complete()
	switch {
	case chk.err != nil:
		s.State, s.Reason, s.Detail = cluster.SyncError, "walk", chk.err.Error()
	case s.Complete:
		s.State = cluster.SyncHeld
		m.mu.Lock()
		m.heldLocked(name, st, work.String())
		m.mu.Unlock()
	default:
		s.State = cluster.SyncSyncing
		m.mu.Lock()
		var err error
		if o := m.fetched[name]; o.target == work {
			err = o.err
		}
		running := m.inflight[name] != nil
		m.mu.Unlock()
		if err != nil {
			var ae *apiv0.Error
			var be *store.BudgetError
			switch {
			case errors.As(err, &be):
				s.State, s.Reason, s.Need, s.Detail = cluster.SyncPaused, "insufficient_storage", be.Need, err.Error()
			case errors.As(err, &ae) && ae.Code == apiv0.CodeInsufficientStorage:
				s.State, s.Reason, s.Detail = cluster.SyncPaused, "insufficient_storage", ae.Error()
				if n, ok := ae.Details["need"].(int64); ok {
					s.Need = n
				}
			default:
				s.Reason, s.Detail = "fetch", err.Error()
			}
		}
		if !running && s.Mode == cluster.ModeOwned && m.cfg.Fetch != nil {
			m.startFetch(name, work, pl, v)
		}
	}
	changed := m.finish(name, s, prev)
	if s.Complete && s.Mode == cluster.ModeOwned && m.cfg.Ready != nil && m.cfg.Ready(name, work) {
		m.mu.Lock()
		if st.Ready != work.String() {
			if st.Ready != "" && !contains(st.Retained, st.Ready) {
				st.Retained = append(st.Retained, st.Ready)
			}
			st.Ready = work.String()
			_ = m.saveLocked()
			if cur := m.status[name]; cur != nil {
				cur.Protection = *st
			}
		}
		m.mu.Unlock()
	}
	return changed
}

// heldLocked records that target is fully held: earlier staged heads are abandoned and
// the previous held version becomes a retained release candidate.
func (m *Manager) heldLocked(name string, st *Protection, target string) {
	if st.Held == target {
		return
	}
	m.abandonLocked(name, st, target)
	if st.Held != "" && st.Held != st.Ready && !contains(st.Retained, st.Held) {
		st.Retained = append(st.Retained, st.Held)
	}
	st.Held = target
	_ = m.saveLocked()
}

// abandonLocked unpins staged heads other than keep that were never fully held or ready
// (bounded staging, S5: not every observed head is kept).
func (m *Manager) abandonLocked(name string, st *Protection, keep string) {
	for _, s := range st.Staged {
		if s == keep {
			continue
		}
		if s == st.Held || s == st.Ready || contains(st.Retained, s) {
			continue
		}
		if c, err := cid.Parse(s); err == nil {
			_, _ = m.cfg.Store.Unpin(pinFor(name, c))
		}
		if f := m.inflight[name]; f != nil && f.target.String() == s {
			f.cancel()
		}
	}
	st.Staged = nil // kept targets are tracked as held, ready or retained
}

func (m *Manager) finish(name string, s *Status, prev *Status) bool {
	m.mu.Lock()
	s.Protection = *m.state[name]
	m.mu.Unlock()
	s.Reported = cluster.Report{Name: name, Target: s.Target.String(), Mode: s.Mode, Policy: s.Policy, Epoch: s.Epoch, Complete: s.Complete,
		Owned: s.Owned, State: s.State}
	if !s.Complete {
		miss := s.Missing
		s.Reported.Missing = &miss
	}
	m.setStatus(name, s)
	return prev == nil || !sameReport(prev.Reported, s.Reported)
}

func sameReport(a, b cluster.Report) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func (m *Manager) setStatus(name string, s *Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s == nil {
		delete(m.status, name)
		return
	}
	m.status[name] = s
}

func (m *Manager) startFetch(name string, work cid.CID, pl *cluster.Placement, v *cluster.View) {
	ctx, cancel := context.WithCancel(m.ctx)
	m.mu.Lock()
	if m.inflight[name] != nil || m.ctx.Err() != nil {
		m.mu.Unlock()
		cancel()
		return
	}
	m.inflight[name] = &fetching{target: work, cancel: cancel}
	m.wg.Add(1)
	m.mu.Unlock()
	self := m.cfg.Self
	members := v.MemberIDs()
	want := func(c cid.CID) bool { return pl.Owns(self, c.Multihash()) }
	rank := func(c cid.CID) []wire.PeerID {
		mh := c.Multihash()
		out := pl.Owners(mh)
		for _, id := range pl.Ranked(mh) {
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
		for _, id := range members {
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
		return out
	}
	go func() {
		defer m.wg.Done()
		err := m.cfg.Fetch(ctx, work, members, want, rank)
		m.mu.Lock()
		delete(m.inflight, name)
		if ctx.Err() == nil {
			m.fetched[name] = fetchOutcome{target: work, err: err}
		}
		m.mu.Unlock()
		cancel()
		if err != nil && ctx.Err() == nil {
			m.log.Debug("placement fetch failed", "dataset", name, "target", work, "err", err)
		}
		m.Kick()
	}()
}

// checkResult is a completeness walk of a target under a placement.
type checkResult struct {
	owned, missing cluster.Counts
	structMissing  int
	blobs          []cid.Multihash
	err            error
}

func (c checkResult) complete() bool {
	return c.err == nil && c.structMissing == 0 && c.missing.Blobs == 0
}

// check walks target's structure in the store: every structure node must be held, and
// every blob this member owns (all blobs when full). It also collects the blobs.
func (m *Manager) check(target cid.CID, size uint64, pl *cluster.Placement, full bool) checkResult {
	var res checkResult
	st := m.cfg.Store
	seen := map[cid.Multihash]bool{}
	type item struct {
		c    cid.CID
		size uint64
	}
	stack := []item{{target, size}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		mh := it.c.Multihash()
		if seen[mh] {
			continue
		}
		seen[mh] = true
		info, ok, err := st.Has(it.c)
		if err != nil {
			res.err = err
			return res
		}
		held := ok && info.Complete
		if it.c.Codec() == cid.Raw {
			res.blobs = append(res.blobs, mh)
			if !full && !pl.Owns(m.cfg.Self, mh) {
				continue
			}
			sz := it.size
			if held {
				sz = info.Size
			}
			res.owned.Blobs++
			res.owned.Bytes += int64(sz)
			if !held {
				res.missing.Blobs++
				res.missing.Bytes += int64(sz)
			}
			continue
		}
		if !held {
			res.structMissing++
			continue
		}
		data, err := st.Get(it.c)
		if err != nil {
			res.err = err
			return res
		}
		links, err := fetch.Links(it.c, data)
		if err != nil {
			res.err = err
			return res
		}
		for i := len(links) - 1; i >= 0; i-- {
			stack = append(stack, item{links[i].CID, links[i].Size})
		}
	}
	return res
}

// Status returns this member's sync status of a dataset.
func (m *Manager) Status(name string) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.status[name]
	if !ok {
		return Status{}, false
	}
	return *s, true
}

// Reports returns this member's completion reports (seq unset).
func (m *Manager) Reports() []cluster.Report {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]cluster.Report, 0, len(m.status))
	for _, s := range m.status {
		out = append(out, s.Reported)
	}
	slices.SortFunc(out, func(a, b cluster.Report) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	return out
}

// Owners computes the owner set of target under dataset name's current policy and epoch
// (E6): the union of the owners of its blobs, or of the target itself when it has no
// blobs. known is false when the target's structure is not held here.
func (m *Manager) Owners(name string, target cid.CID) (owners []wire.PeerID, pl *cluster.Placement, known bool) {
	v := m.cfg.Cluster.View()
	d, ok := v.Dataset(name)
	if !ok {
		return nil, nil, false
	}
	pl = cluster.NewPlacement(v, d.Policy, m.cfg.Self)
	chk := m.check(target, 0, pl, true)
	if chk.err != nil || chk.structMissing > 0 {
		return nil, pl, false
	}
	set := map[wire.PeerID]bool{}
	if len(chk.blobs) == 0 {
		chk.blobs = []cid.Multihash{target.Multihash()}
	}
	for _, mh := range chk.blobs {
		for _, id := range pl.Owners(mh) {
			set[id] = true
		}
	}
	for id := range set {
		owners = append(owners, id)
	}
	slices.Sort(owners)
	return owners, pl, true
}

// Protected lists the targets a dataset keeps pinned: the last ready and held versions,
// staged heads and retained release candidates (release.auto is off: nothing is
// released, docs/operations.md S8).
func (m *Manager) Protected(name string) (st Protection) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.state[name]; s != nil {
		st = *s
		st.Staged = slices.Clone(s.Staged)
		st.Retained = slices.Clone(s.Retained)
	}
	return st
}
