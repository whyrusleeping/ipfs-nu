package replica

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// Safe release (docs/operations.md S1–S4, S6, S8; docs/cluster.md "Sync, handoff and
// repair"). A member releases two kinds of candidates:
//
//   - version: an earlier target it keeps pinned (the previous held or ready head, staged
//     heads, a draining member's held view). It is unpinned, so GC may reclaim what no
//     other pin reaches, once every blob of the current head (and live set) has k
//     confirmed copies from its owners in the current epoch (the protected-version rule).
//   - surplus: a blob of the current head or live set that it holds but does not own in
//     the current epoch (after an epoch change, or every blob while draining). It is
//     dropped once its owners other than this member confirm it k times (S2), and only if
//     every other dataset reaching it would release it too and no other pin reaches it.
//
// Every release waits until the binding (head, live set, policy, epoch) has been this
// member's merged state for release.settle, no record is held back, no member reports
// another epoch or policy, and no concurrent head is unsettled (S3, S4). Automatic
// release (release.auto) additionally waits dataset.remove_grace per surplus blob.

// ReleaseSettings are the member's release settings.
type ReleaseSettings struct {
	Auto          bool
	Settle, Grace time.Duration
	Fresh         time.Duration // evidence.fresh
}

type boundAt struct {
	key   string
	since time.Time
}

func (m *Manager) settings() ReleaseSettings {
	var rs ReleaseSettings
	if m.cfg.Release != nil {
		rs = m.cfg.Release()
	}
	if rs.Settle <= 0 {
		rs.Settle = cluster.DefaultSettle
	}
	if rs.Grace < 0 {
		rs.Grace = 0
	}
	if rs.Fresh <= 0 {
		rs.Fresh = cluster.DefaultFresh
	}
	return rs
}

// noteMembers records when each member's state was first seen as it is now (S3).
func (m *Manager) noteMembers(v *cluster.View) {
	now := m.cfg.Cluster.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, rec := range v.Members {
		st := "deleted"
		if !rec.Deleted && rec.Value != nil {
			st = rec.Value.State
		}
		if s, ok := m.memberSeen[id]; !ok || s.key != st {
			m.memberSeen[id] = boundAt{key: st, since: now}
		}
	}
}

func (m *Manager) memberSince(id wire.PeerID) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.memberSeen[id]; ok {
		return s.since
	}
	return m.cfg.Cluster.Now()
}

// plan is one dataset's release decision.
type plan struct {
	name     string
	rep      apiv0.ReleaseReport
	versions []cid.CID                // version candidates that pass
	surplus  map[cid.Multihash]bool   // surplus blobs that pass
	since    map[cid.Multihash]string // CID strings of passing surplus blobs
	reach    map[cid.Multihash]bool   // blobs any pin of the dataset reaches
	sizes    map[cid.Multihash]int64
}

// planFor evaluates one dataset. ok is false when there is nothing to decide (no head,
// not placed here).
func (m *Manager) planFor(v *cluster.View, name string, d *cluster.Dataset, rs ReleaseSettings) (*plan, bool) {
	head, ok := v.Head(name)
	if !ok {
		return nil, false
	}
	target, err := cid.Parse(head.Value.Target)
	if err != nil {
		return nil, false
	}
	now := m.cfg.Cluster.Now()
	self := m.cfg.Self
	pl := cluster.NewPlacement(v, d.Policy, self)
	p := &plan{name: name, surplus: map[cid.Multihash]bool{}, since: map[cid.Multihash]string{}, reach: map[cid.Multihash]bool{}, sizes: map[cid.Multihash]int64{},
		rep: apiv0.ReleaseReport{Dataset: name, Auto: rs.Auto, Target: target.String(), Epoch: pl.Epoch, Policy: d.PolicyHash(),
			Deferred: []string{}, Candidates: []apiv0.ReleaseCandidate{}}}
	roots := []cid.CID{target}
	liveRoots := map[string]bool{target.String(): true}
	if d.Policy.History != nil {
		hist := v.VersionHistory(name)
		ret := cluster.Retain(name, hist, d.Policy.History, cluster.HeadVersion(hist, head.Value.Target))
		p.rep.Live = ret.ID
		for _, e := range hist {
			if _, live := ret.Live[e.TID]; live && !liveRoots[e.Root] {
				if c, err := cid.Parse(e.Root); err == nil {
					roots = append(roots, c)
					liveRoots[e.Root] = true
				}
			}
		}
	}
	m.mu.Lock()
	st := m.state[name]
	if st == nil {
		m.mu.Unlock()
		return nil, false
	}
	key := strings.Join([]string{target.String(), p.rep.Live, p.rep.Policy, p.rep.Epoch}, "|")
	b, seen := m.bound[name]
	if !seen || b.key != key {
		b = boundAt{key: key, since: now}
		m.bound[name] = b
	}
	ev := map[wire.PeerID]cluster.Evidence{}
	if s := m.status[name]; s != nil {
		ev[self] = cluster.Evidence{Report: s.Reported, Received: now}
	}
	prot := st.clone()
	m.mu.Unlock()
	if m.cfg.Evidence != nil {
		for id, e := range m.cfg.Evidence(name) {
			if id != self {
				ev[id] = e
			}
		}
	}
	in := cluster.ReleaseInput{Target: target.String(), Live: p.rep.Live, Policy: p.rep.Policy, Epoch: p.rep.Epoch, K: pl.K(), Self: self,
		Evidence: ev, Now: now, Fresh: rs.Fresh, Settle: rs.Settle, BoundSince: b.since, HeldBack: v.HeldBack, Members: v.IsMember, MemberSince: m.memberSince}
	for _, h := range v.Heads[name] {
		if !h.Deleted && h.Value != nil && h.Value.Target != target.String() && now.Sub(h.HLC.Time()) < rs.Settle {
			in.Displaced = append(in.Displaced, h.Value.Target)
		}
	}
	slices.Sort(in.Displaced)
	p.rep.Deferred = append(p.rep.Deferred, in.Deferred()...)
	settled := len(p.rep.Deferred) == 0

	// The current head and live set: every blob, held or not, by owner group.
	type group struct {
		owners []wire.PeerID
		blobs  []cid.Multihash
	}
	groups := map[string]*group{}
	headBlobs := map[cid.Multihash]bool{}
	structOK := true
	for _, r := range roots {
		chk := m.check(r, 0, pl, true)
		if chk.err != nil || chk.structMissing > 0 {
			structOK = false
			continue
		}
		blobs := chk.blobs
		if len(blobs) == 0 {
			blobs = []cid.Multihash{r.Multihash()}
		}
		for _, mh := range blobs {
			if headBlobs[mh] {
				continue
			}
			headBlobs[mh] = true
			if sz, ok := chk.held[mh]; ok {
				p.sizes[mh] = sz
			}
			owners := pl.Owners(mh)
			k := ""
			for _, o := range owners {
				k += string(o) + "\x00"
			}
			g := groups[k]
			if g == nil {
				g = &group{owners: owners}
				groups[k] = g
			}
			g.blobs = append(g.blobs, mh)
		}
	}
	// Head confirmation (version candidates): every group has k owner confirmations,
	// this member's own complete report included.
	headOK := structOK && len(groups) > 0 && pl.Feasible().OK
	var headShort []apiv0.MissingOwner
	var headConf []string
	for _, g := range groups {
		conf, miss := in.Confirm(g.owners, true)
		if len(conf) < in.K {
			headOK = false
			headShort = appendMissing(headShort, miss)
		}
		for _, c := range conf {
			if !slices.Contains(headConf, c.Base36()) {
				headConf = append(headConf, c.Base36())
			}
		}
	}
	if !structOK {
		headShort = append(headShort, apiv0.MissingOwner{Peer: self.Base36(), Reason: "structure of the current head is not held here"})
	}
	slices.Sort(headConf)
	// A member outside placement (draining) releases everything it holds, the head
	// included; an eligible one keeps the head, the live set and retained versions
	// (those leave by an explicit prune).
	eligible := pl.Eligible(self)
	cands := map[string]bool{}
	pinned := append(append([]string{prot.Ready, prot.Held}, prot.Staged...), prot.Retained...)
	if !eligible {
		for _, r := range prot.Versions {
			pinned = append(pinned, r)
		}
	}
	for _, r := range pinned {
		if r != "" && !(eligible && liveRoots[r]) && !slices.Contains(in.Displaced, r) {
			cands[r] = true
		}
	}
	if eligible {
		for _, r := range prot.Versions {
			delete(cands, r)
		}
	}
	var vs []string
	for r := range cands {
		vs = append(vs, r)
	}
	slices.Sort(vs)
	for _, r := range vs {
		c, err := cid.Parse(r)
		if err != nil {
			continue
		}
		cand := apiv0.ReleaseCandidate{Kind: "version", CID: r, OK: headOK && settled, Confirmed: headConf, Shortfall: headShort}
		chk := m.check(c, 0, pl, true)
		for mh, sz := range chk.held {
			if !headBlobs[mh] {
				cand.Blobs++
				cand.Bytes += sz
			}
		}
		if cand.Confirmed == nil {
			cand.Confirmed = []string{}
		}
		if cand.Shortfall == nil {
			cand.Shortfall = []apiv0.MissingOwner{}
		}
		p.rep.Candidates = append(p.rep.Candidates, cand)
		if cand.OK {
			p.versions = append(p.versions, c)
		}
	}
	// Surplus: held blobs of the head and live set this member does not own.
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		g := groups[k]
		var held []cid.Multihash
		for _, mh := range g.blobs {
			if _, ok := p.sizes[mh]; ok && !pl.Owns(self, mh) {
				held = append(held, mh)
			}
		}
		if len(held) == 0 {
			continue
		}
		conf, miss := in.Confirm(g.owners, false)
		cand := apiv0.ReleaseCandidate{Kind: "surplus", CID: cid.New(cid.Raw, held[0]).String(), Blobs: int64(len(held)), OK: settled && len(conf) >= in.K,
			Confirmed: []string{}, Shortfall: appendMissing(nil, miss)}
		if len(conf) < in.K && len(miss) == 0 {
			cand.Shortfall = append(cand.Shortfall, apiv0.MissingOwner{Peer: "-", Reason: "fewer owners than replicas (policy unsatisfiable)"})
		}
		if cand.Shortfall == nil {
			cand.Shortfall = []apiv0.MissingOwner{}
		}
		for _, c := range conf {
			cand.Confirmed = append(cand.Confirmed, c.Base36())
		}
		var first int64
		for _, mh := range held {
			cand.Bytes += p.sizes[mh]
			if cand.OK {
				p.surplus[mh] = true
				cs := cid.New(cid.Raw, mh).String()
				p.since[mh] = cs
				if t, ok := prot.Releasing[cs]; ok && (first == 0 || t < first) {
					first = t
				}
			}
		}
		if cand.OK && rs.Auto {
			if first == 0 {
				first = now.UnixMilli()
			}
			t := time.UnixMilli(first).Add(rs.Grace)
			cand.PendingUntil = &t
		}
		p.rep.Candidates = append(p.rep.Candidates, cand)
	}
	// Everything the dataset's pins reach (cross-dataset check of surplus drops).
	pins, _ := m.cfg.Store.Pins()
	for _, pin := range pins {
		if pin.Source == store.SourceCluster && pin.Name == "dataset/"+name {
			_, _ = m.cfg.Store.Walk(m.ctx, pin, func(c cid.CID, held bool) error {
				if held && c.Codec() == cid.Raw {
					p.reach[c.Multihash()] = true
				}
				return nil
			})
		}
	}
	return p, true
}

func appendMissing(out []apiv0.MissingOwner, miss []cluster.MissingOwner) []apiv0.MissingOwner {
	for _, mo := range miss {
		e := apiv0.MissingOwner{Peer: mo.Peer.Base36(), Reason: mo.Reason}
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}

// ReleaseReport evaluates (dryRun) or applies safe release for one dataset now. An
// explicit apply releases what passes S1–S4 without waiting for dataset.remove_grace.
func (m *Manager) ReleaseReport(name string, dryRun bool) (apiv0.ReleaseReport, bool) {
	m.rmu.Lock()
	defer m.rmu.Unlock()
	v := m.cfg.Cluster.View()
	m.noteMembers(v)
	return m.releasePass(v, name, !dryRun)
}

// releasePass plans every placed dataset (surplus drops need all of them) and applies
// the releases of only (every dataset when ""): explicit applies at once, otherwise
// automatic release with dataset.remove_grace. It returns only's report.
func (m *Manager) releasePass(v *cluster.View, only string, explicit bool) (apiv0.ReleaseReport, bool) {
	rs := m.settings()
	apply := explicit || (rs.Auto && only == "")
	plans := map[string]*plan{}
	for name, sd := range v.Datasets {
		if sd.Deleted || !Placed(sd.Value) {
			continue
		}
		if p, ok := m.planFor(v, name, sd.Value, rs); ok {
			plans[name] = p
		}
	}
	now := m.cfg.Cluster.Now()
	var drop []cid.Multihash
	owner := map[cid.Multihash]string{}
	for name, p := range plans {
		p.rep.DryRun = !apply
		if !apply || (only != "" && name != only) {
			continue
		}
		m.mu.Lock()
		st := m.state[name]
		if st.Releasing == nil {
			st.Releasing = map[string]int64{}
		}
		for cs := range st.Releasing { // no longer passing: start over
			c, err := cid.Parse(cs)
			if err != nil || !p.surplus[c.Multihash()] {
				delete(st.Releasing, cs)
			}
		}
		for mh := range p.surplus {
			cs := p.since[mh]
			if _, ok := st.Releasing[cs]; !ok {
				st.Releasing[cs] = now.UnixMilli()
			}
			if !explicit && now.Sub(time.UnixMilli(st.Releasing[cs])) < rs.Grace {
				continue
			}
			// Another dataset reaching the blob must release it too.
			ok := true
			for other, q := range plans {
				if other != name && q.reach[mh] && !q.surplus[mh] {
					ok = false
					break
				}
			}
			for other, sd := range v.Datasets { // datasets without a plan here keep it
				if _, planned := plans[other]; !planned && other != name && !sd.Deleted && m.reaches(other, mh) {
					ok = false
					break
				}
			}
			if ok {
				drop = append(drop, mh)
				owner[mh] = name
			}
		}
		for _, c := range p.versions {
			if _, err := m.cfg.Store.Unpin(pinFor(name, c)); err != nil {
				continue
			}
			cs := c.String()
			st.Retained = slices.DeleteFunc(st.Retained, func(r string) bool { return r == cs })
			st.Staged = slices.DeleteFunc(st.Staged, func(r string) bool { return r == cs })
			if st.Ready == cs {
				st.Ready = p.rep.Target
			}
			if st.Held == cs {
				st.Held = ""
			}
			p.rep.Released = append(p.rep.Released, cs)
			m.log.Info("released version", "dataset", name, "target", cs)
		}
		_ = m.saveLocked()
		m.mu.Unlock()
	}
	if len(drop) > 0 {
		dropped, err := m.cfg.Store.Drop(m.ctx, drop, func(pin store.Pin) bool {
			return pin.Source != store.SourceCluster || !strings.HasPrefix(pin.Name, "dataset/")
		})
		if err != nil {
			m.log.Warn("surplus release failed", "err", err)
		}
		m.mu.Lock()
		for _, mh := range dropped {
			p := plans[owner[mh]]
			p.rep.ReleasedBlobs++
			p.rep.ReleasedBytes += p.sizes[mh]
			delete(m.state[p.name].Releasing, p.since[mh])
		}
		_ = m.saveLocked()
		m.mu.Unlock()
		if len(dropped) > 0 {
			m.log.Info("released surplus blobs", "blobs", len(dropped))
		}
	}
	for _, p := range plans {
		if len(p.rep.Released) > 0 || p.rep.ReleasedBlobs > 0 {
			m.Kick()
			break
		}
	}
	if p := plans[only]; p != nil {
		return p.rep, true
	}
	return apiv0.ReleaseReport{}, false
}

// reaches reports whether a dataset's pins reach a held blob.
func (m *Manager) reaches(name string, mh cid.Multihash) bool {
	pins, _ := m.cfg.Store.Pins()
	found := false
	for _, pin := range pins {
		if found || pin.Source != store.SourceCluster || pin.Name != "dataset/"+name {
			continue
		}
		_, _ = m.cfg.Store.Walk(context.Background(), pin, func(c cid.CID, _ bool) error {
			if c.Multihash() == mh {
				found = true
			}
			return nil
		})
	}
	return found
}
