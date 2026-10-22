// Package replica runs cluster placement on a member (docs/cluster.md "Placement",
// "Sync, handoff and repair", "Status"): for every dataset the member is eligible for it
// pins the head, fetches its structure and the blobs it owns in the current placement
// epoch from members in placement order, checks completeness, and produces the
// completion reports heartbeats carry. It keeps the protected versions of
// docs/operations.md S5/S7 pinned and releases only under S1–S4 (release.go): reported
// by `gc --dry-run`, applied by an explicit `gc` or, with release.auto, automatically.
package replica

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/atrepo"
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
	// Evidence returns other members' reports for a dataset (release evidence, S2); nil:
	// none, so nothing is ever released by evidence. Release returns the release
	// settings (nil: release.auto off, defaults). See release.go.
	Evidence func(name string) map[wire.PeerID]cluster.Evidence
	Release  func() ReleaseSettings
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
	// Versions are the version roots (tid → root) pinned for retention (datasets with
	// history); Complete the versions established complete (locally, or by an archive-ready
	// evaluation); Pruned the versions released by an explicit prune.
	Versions map[string]string `json:"versions,omitempty"`
	Complete []string          `json:"complete,omitempty"`
	Pruned   []string          `json:"pruned,omitempty"`
	// Releasing records when each surplus blob (CID) first passed S1–S4 (release.auto
	// drops it dataset.remove_grace later if it still passes).
	Releasing map[string]int64 `json:"releasing,omitempty"`
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
	// HoldsAll says every blob of the target (and of every live version) is held, not only
	// the owned ones: the report is then in mode full and confirms every blob (E6).
	HoldsAll       bool
	Reason, Detail string
	Need           int64 // bytes needed when paused for storage (S7)
	Protection     Protection
	// Live is the live-set identity of a dataset with history; LiveVersions its versions
	// (tid → whether this member holds the version's structure and owned blobs).
	Live         string
	LiveVersions map[string]bool
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

	rmu      sync.Mutex // serializes Reconcile
	mu       sync.Mutex
	state    map[string]*Protection
	status   map[string]*Status
	inflight map[string]*fetching
	fetched  map[string]fetchOutcome
	// Release and scrub state (release.go, scrub.go), in memory: a restart starts every
	// settle period afresh (conservative).
	bound      map[string]boundAt
	memberSeen map[wire.PeerID]boundAt
	scrub      *apiv0.ScrubReport
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
		inflight: map[string]*fetching{}, fetched: map[string]fetchOutcome{},
		bound: map[string]boundAt{}, memberSeen: map[wire.PeerID]boundAt{}}
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

// Placed reports whether this manager places a dataset: a non-collection dataset with
// replicas > 0 or all. Collections are floor-placed by whole items in the collection
// manager.
func Placed(d *cluster.Dataset) bool {
	return d != nil && d.Collection == nil && (d.Policy.Replicas > 0 || d.Policy.All)
}

func pinFor(name string, c cid.CID) store.Pin {
	return store.Pin{CID: c, Mode: store.Recursive, Profile: "nu-tree", Source: store.SourceCluster, Name: "dataset/" + name}
}

// Profile is the walk profile a dataset's content is pinned and checked under: nu-tree,
// or for an atproto-account source (0054) atrepo (atrepo+blobs when blobs are selected,
// so GC keeps them; completeness still means structure and records, since blobs out of
// the 8 MiB limit are skipped by design).
func Profile(d *cluster.Dataset) string {
	switch {
	case d == nil || d.Source.Atrepo == "":
		return "nu-tree"
	case d.Source.Blobs:
		return "atrepo+blobs"
	}
	return "atrepo"
}

func pinWith(name string, c cid.CID, profile string) store.Pin {
	p := pinFor(name, c)
	p.Profile = profile
	return p
}

// Reconcile runs one pass over every dataset (passes are serialized).
func (m *Manager) Reconcile() {
	m.rmu.Lock()
	defer m.rmu.Unlock()
	v := m.cfg.Cluster.View()
	m.noteMembers(v)
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
	if m.settings().Auto {
		m.releasePass(v, "", false)
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
	prof := Profile(d)
	if err := m.cfg.Store.Pin(pinWith(name, work, prof)); err != nil {
		s.State, s.Reason, s.Detail = cluster.SyncError, "pin", err.Error()
		return m.finish(name, s, prev)
	}
	workSize := uint64(0)
	if work == target {
		workSize = size
	}

	chk := m.check(work, workSize, pl, s.Mode == cluster.ModeFull, prof)
	s.Owned, s.Missing, s.StructureMissing, s.Complete = chk.owned, chk.missing, chk.structMissing, chk.complete()
	s.HoldsAll = s.Complete && chk.allHeld
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
	if d.Policy.History != nil {
		m.history(v, name, d, s, st, pl, work)
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
				cur.Protection = st.clone()
			}
		}
		m.mu.Unlock()
	}
	return changed
}

// history pins and checks the live versions of a dataset with history (docs/cluster.md
// "Retention"): every live version is an acquisition obligation, fetched once the work
// target is complete, newest first; versions that left the live set without ever being
// complete here are unpinned (skipped), complete ones stay pinned (release candidates,
// released only by an explicit prune).
func (m *Manager) history(v *cluster.View, name string, d *cluster.Dataset, s *Status, st *Protection, pl *cluster.Placement, work cid.CID) {
	hist := v.VersionHistory(name)
	head, _ := v.Head(name)
	ret := cluster.Retain(name, hist, d.Policy.History, cluster.HeadVersion(hist, head.Value.Target))
	s.Live, s.LiveVersions = ret.ID, map[string]bool{}
	full := s.Mode == cluster.ModeFull
	local := m.cfg.Cluster.Mode() == cluster.ModeLocal
	prof := Profile(d)
	m.mu.Lock()
	if st.Versions == nil {
		st.Versions = map[string]string{}
	}
	m.mu.Unlock()
	var fetchNext cid.CID
	liveRoots := map[string]bool{}
	for i := len(hist) - 1; i >= 0; i-- {
		e := hist[i]
		if _, ok := ret.Live[e.TID]; !ok {
			continue
		}
		liveRoots[e.Root] = true
		root, err := cid.Parse(e.Root)
		if err != nil {
			continue
		}
		var complete bool
		if root == work {
			complete = s.Complete
		} else {
			if err := m.cfg.Store.Pin(pinWith(name, root, prof)); err != nil {
				s.LiveVersions[e.TID] = false
				s.HoldsAll = false
				continue
			}
			chk := m.check(root, 0, pl, full, prof)
			complete = chk.complete()
			s.HoldsAll = s.HoldsAll && complete && chk.allHeld
			if !complete && !fetchNext.Defined() {
				fetchNext = root
			}
		}
		s.LiveVersions[e.TID] = complete
		m.mu.Lock()
		st.Versions[e.TID] = e.Root
		// On the one-node cluster local completeness establishes a version; elsewhere an
		// archive-ready evaluation does (Establish).
		if complete && local && !contains(st.Complete, e.TID) {
			st.Complete = append(st.Complete, e.TID)
		}
		st.Pruned = slices.DeleteFunc(st.Pruned, func(t string) bool { return t == e.TID })
		m.mu.Unlock()
	}
	m.mu.Lock()
	known := map[string]bool{}
	for _, e := range hist {
		known[e.TID] = true
	}
	for tid, root := range st.Versions {
		if _, live := ret.Live[tid]; live {
			continue
		}
		if !known[tid] { // compacted metadata
			delete(st.Versions, tid)
			continue
		}
		if contains(st.Complete, tid) || m.protectedLocked(st, root, liveRoots) {
			continue
		}
		c, err := cid.Parse(root)
		if err == nil && !local && m.check(c, 0, pl, full, prof).complete() {
			continue // this member's share is held: a release candidate, not skipped
		}
		// Skipped: never complete here and no longer selected.
		if err == nil {
			_, _ = m.cfg.Store.Unpin(pinFor(name, c))
		}
		delete(st.Versions, tid)
	}
	st.Complete = slices.DeleteFunc(st.Complete, func(t string) bool { return !known[t] })
	_ = m.saveLocked()
	running := m.inflight[name] != nil
	m.mu.Unlock()
	if fetchNext.Defined() && s.Complete && !running && !full && m.cfg.Fetch != nil {
		m.startFetch(name, fetchNext, pl, v)
	}
}

// protectedLocked reports whether root is protected for another reason than a version
// (current, ready, held, staged or retained target, or a live version's root).
func (m *Manager) protectedLocked(st *Protection, root string, liveRoots map[string]bool) bool {
	return liveRoots[root] || root == st.Ready || root == st.Held || contains(st.Staged, root) || contains(st.Retained, root)
}

// Establish records versions as established complete (an archive-ready evaluation
// covered them).
func (m *Manager) Establish(name string, tids []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state[name]
	if st == nil {
		return
	}
	changed := false
	for _, t := range tids {
		if !contains(st.Complete, t) {
			st.Complete = append(st.Complete, t)
			changed = true
		}
	}
	if changed {
		_ = m.saveLocked()
	}
}

// Prune releases the pins of versions (tid → root) that are not protected: not the
// head or any live version's root (headRoot, live) nor the ready, held, staged target.
// Released versions are recorded as pruned. It returns the tids released.
func (m *Manager) Prune(name string, versions map[string]string, headRoot string, live map[string]bool) ([]string, error) {
	m.rmu.Lock() // not concurrently with a reconcile pass
	defer m.rmu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state[name]
	if st == nil {
		return nil, nil
	}
	var out []string
	for tid, root := range versions {
		if root == headRoot || live[root] || root == st.Ready || root == st.Held || contains(st.Staged, root) {
			continue
		}
		c, err := cid.Parse(root)
		if err != nil {
			continue
		}
		if _, err := m.cfg.Store.Unpin(pinFor(name, c)); err != nil {
			return out, err
		}
		delete(st.Versions, tid)
		st.Retained = slices.DeleteFunc(st.Retained, func(r string) bool { return r == root })
		st.Complete = slices.DeleteFunc(st.Complete, func(t string) bool { return t == tid })
		if !contains(st.Pruned, tid) {
			st.Pruned = append(st.Pruned, tid)
		}
		out = append(out, tid)
	}
	slices.Sort(out)
	return out, m.saveLocked()
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
	s.Protection = m.state[name].clone()
	m.mu.Unlock()
	mode := s.Mode
	if s.Complete && s.HoldsAll {
		mode = cluster.ModeFull // every blob is held: confirms them all
	}
	s.Reported = cluster.Report{Name: name, Target: s.Target.String(), Mode: mode, Policy: s.Policy, Epoch: s.Epoch, Complete: s.Complete,
		Owned: s.Owned, State: s.State, Live: s.Live}
	if s.Live != "" {
		for tid, ok := range s.LiveVersions {
			if !ok {
				s.Reported.Incomplete = append(s.Reported.Incomplete, tid)
			}
		}
		slices.Sort(s.Reported.Incomplete)
		if len(s.Reported.Incomplete) > cluster.MaxIncomplete {
			s.Reported.Incomplete = s.Reported.Incomplete[:cluster.MaxIncomplete]
		}
	}
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

// checkResult is a completeness walk of a target under a placement; allHeld says every
// blob is held, owned or not.
type checkResult struct {
	owned, missing cluster.Counts
	structMissing  int
	allHeld        bool
	blobs          []cid.Multihash
	held           map[cid.Multihash]int64 // held blobs with their sizes
	err            error
}

func (c checkResult) complete() bool {
	return c.err == nil && c.structMissing == 0 && c.missing.Blobs == 0
}

// check walks target's structure in the store under profile: every structure node must
// be held, and every blob this member owns (all blobs when full). It also collects the
// blobs. Under atrepo profiles records are leaves and blobs are not walked.
func (m *Manager) check(target cid.CID, size uint64, pl *cluster.Placement, full bool, profile string) checkResult {
	res := checkResult{allHeld: true, held: map[cid.Multihash]int64{}}
	links := fetch.Links
	if strings.HasPrefix(profile, "atrepo") {
		links = func(c cid.CID, data []byte) ([]fetch.Link, error) {
			ls, err := atrepo.Links(c, data)
			out := make([]fetch.Link, len(ls))
			for i, l := range ls {
				out[i] = fetch.Link{CID: l}
			}
			return out, err
		}
	}
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
			res.allHeld = res.allHeld && held
			if held {
				res.held[mh] = int64(info.Size)
			}
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
		kids, err := links(it.c, data)
		if err != nil {
			res.err = err
			return res
		}
		for i := len(kids) - 1; i >= 0; i-- {
			stack = append(stack, item{kids[i].CID, kids[i].Size})
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
	out := *s
	out.LiveVersions = maps.Clone(s.LiveVersions)
	return out, true
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

// Groups computes the blobs of roots (deduplicated) by owner set under dataset name's
// current policy and epoch (E6); a target without blobs counts its own multihash as the
// unit, so an empty tree still needs confirming members. known is false when some root's
// structure is not held here.
func (m *Manager) Groups(name string, roots ...cid.CID) (groups []cluster.OwnerGroup, pl *cluster.Placement, known bool) {
	v := m.cfg.Cluster.View()
	d, ok := v.Dataset(name)
	if !ok {
		return nil, nil, false
	}
	pl = cluster.NewPlacement(v, d.Policy, m.cfg.Self)
	var gs cluster.GroupSet
	seen := map[cid.Multihash]bool{}
	for _, root := range roots {
		chk := m.check(root, 0, pl, true, Profile(d))
		if chk.err != nil || chk.structMissing > 0 {
			return nil, pl, false
		}
		if len(chk.blobs) == 0 {
			chk.blobs = []cid.Multihash{root.Multihash()}
		}
		for _, mh := range chk.blobs {
			if !seen[mh] {
				seen[mh] = true
				gs.Add(pl.Owners(mh), 1)
			}
		}
	}
	return gs.List(), pl, true
}

func (p *Protection) clone() Protection {
	st := *p
	st.Staged = slices.Clone(p.Staged)
	st.Retained = slices.Clone(p.Retained)
	st.Versions = maps.Clone(p.Versions)
	st.Complete = slices.Clone(p.Complete)
	st.Pruned = slices.Clone(p.Pruned)
	st.Releasing = maps.Clone(p.Releasing)
	return st
}

// Protected lists the targets a dataset keeps pinned: the last ready and held versions,
// staged heads and retained release candidates (release.auto is off: nothing is
// released, docs/operations.md S8).
func (m *Manager) Protected(name string) (st Protection) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.state[name]; s != nil {
		st = s.clone()
	}
	return st
}
