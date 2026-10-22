package node

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/collection"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/replica"
)

// Placement, readiness and waits (docs/cluster.md "Placement", "Status";
// docs/operations.md "Readiness and status").

// PlacedFetcher is implemented by fetchers that fetch for cluster placement (the swarm).
type PlacedFetcher interface {
	FetchPlaced(ctx context.Context, root cid.CID, members []wire.PeerID, want func(cid.CID) bool, rank func(cid.CID) []wire.PeerID, have []cid.CID) error
}

// ProfilePlacedFetcher carries atrepo blob policy and held commits to the swarm.
type ProfilePlacedFetcher interface {
	FetchPlacedProfile(context.Context, cid.CID, []wire.PeerID, func(cid.CID) bool, func(cid.CID) []wire.PeerID, []cid.CID, string) error
}

// WaitPoll is how often a wait re-evaluates (tests shorten it).
var WaitPoll = 100 * time.Millisecond

// replicas holds the placement managers of the node's clusters and the readiness marks.
type replicas struct {
	mu    sync.Mutex
	mgrs  map[*cluster.Cluster]*replica.Manager
	ready map[string]readyMark // cluster ID/dataset → last ready evaluation
	// unknown records since when a dataset's readiness has been unknown (E8 alert).
	unknown map[string]time.Time
}

type readyMark struct {
	target, epoch string
	age           time.Duration
	at            time.Time
}

// replicaFor returns c's placement manager, opening it (not started) on first use.
func (n *Local) replicaFor(c *cluster.Cluster) *replica.Manager {
	r := &n.reps
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.mgrs[c]; m != nil {
		return m
	}
	if r.mgrs == nil {
		r.mgrs = map[*cluster.Cluster]*replica.Manager{}
	}
	cfg := replica.Config{Store: n.Store, Cluster: c, Self: n.PeerID(), Interval: ClusterInterval,
		Ready: func(name string, target cid.CID) bool {
			n.establishArchive(c, name)
			return n.evalContent(c, name, target).cs.State == cluster.ReadyReady
		},
		Changed: func() {
			if svc := n.clusters.svcPtr.Load(); svc != nil && c.Mode() != cluster.ModeLocal {
				svc.KickHeartbeat()
			}
		}}
	if c.Mode() != cluster.ModeLocal {
		cfg.Evidence, cfg.Release = n.remoteEvidence, n.releaseSettings
	}
	if c.Mode() == cluster.ModeLocal {
		cfg.Dir = n.clusterDir("local")
	} else {
		cfg.Dir = n.clusterDir("net")
	}
	if f := n.Fetcher; f != nil {
		if pf, ok := f.(PlacedFetcher); ok && c.Mode() != cluster.ModeLocal {
			if ppf, ok := f.(ProfilePlacedFetcher); ok {
				cfg.FetchProfile = ppf.FetchPlacedProfile
			}
			cfg.Fetch = func(ctx context.Context, root cid.CID, members []wire.PeerID, want func(cid.CID) bool, rank func(cid.CID) []wire.PeerID) error {
				return pf.FetchPlaced(ctx, root, members, want, rank, nil)
			}
		} else if _, none := f.(NoFetcher); !none {
			cfg.Fetch = func(ctx context.Context, root cid.CID, _ []wire.PeerID, _ func(cid.CID) bool, _ func(cid.CID) []wire.PeerID) error {
				return f.Fetch(ctx, root, nil, nil)
			}
		}
	}
	m, err := replica.Open(cfg)
	if err != nil {
		return nil
	}
	r.mgrs[c] = m
	return m
}

// startReplica runs c's placement manager in the background (daemon/attached nodes).
func (n *Local) startReplica(c *cluster.Cluster) {
	if m := n.replicaFor(c); m != nil {
		m.Start()
	}
}

// closeReplicas stops every placement manager.
func (n *Local) closeReplicas() {
	r := &n.reps
	r.mu.Lock()
	mgrs := r.mgrs
	r.mgrs = nil
	r.mu.Unlock()
	for _, m := range mgrs {
		_ = m.Close()
	}
}

// reportsOf returns c's completion reports for heartbeats: placed datasets and, from
// coll (nil: none), collection floor placement.
func (n *Local) reportsOf(c *cluster.Cluster, coll *collection.Manager) func() []cluster.Report {
	return func() []cluster.Report {
		var out []cluster.Report
		if m := n.replicaFor(c); m != nil {
			out = m.Reports()
		}
		if coll != nil {
			out = append(out, coll.Reports()...)
		}
		return out
	}
}

// evalOut is a readiness evaluation with its evidence.
type evalOut struct {
	cs       apiv0.ContentState
	evidence map[wire.PeerID]cluster.Evidence
	now      time.Time
	fresh    time.Duration
}

func peerStr(id wire.PeerID) string { return id.Base36() }

func confirmations(cs []cluster.Confirmation) []apiv0.Confirmation {
	out := []apiv0.Confirmation{}
	for _, c := range cs {
		out = append(out, apiv0.Confirmation{Peer: peerStr(c.Peer), Seq: c.Seq, AgeMS: c.Age.Milliseconds()})
	}
	return out
}

// evalContent evaluates readiness of target for dataset name in c (E5–E8) as this node
// sees it, with its own sync state and protected versions.
func (n *Local) evalContent(c *cluster.Cluster, name string, target cid.CID) evalOut {
	return n.evalTarget(c, name, target, nil)
}

// evalTarget is evalContent, archive-wide over a live set when live is set: the owner set
// is the union over the live versions and reports must be bound to the live set (E8).
func (n *Local) evalTarget(c *cluster.Cluster, name string, target cid.CID, live *liveBind) evalOut {
	v := c.View()
	out := evalOut{now: c.Now(), fresh: EvidenceFresh, evidence: map[wire.PeerID]cluster.Evidence{}}
	cs := apiv0.ContentState{Target: target.String(), Mode: cluster.ModeOwned, Confirmed: []apiv0.Confirmation{}, Missing: []apiv0.MissingOwner{},
		Extra: []apiv0.Confirmation{}, Unmet: []string{}}
	d, ok := v.Dataset(name)
	if !ok {
		cs.State, cs.Detail = cluster.ReadyUnknown, "no such dataset"
		out.cs = cs
		return out
	}
	cs.Policy = d.PolicyHash()
	var (
		groups []cluster.OwnerGroup
		pl     *cluster.Placement
		known  bool
		unit   = "blob"
	)
	self := n.PeerID()
	var m *replica.Manager
	if d.Collection != nil {
		// Floor placement: items are the units, bound to the merged catalog root.
		if d.Policy.Replicas == 0 && !d.Policy.All {
			cs.State, cs.Detail = cluster.ReadyUnsatisfied, "a collection with replicas 0 has no guaranteed copies; see `"+nu.Name+" cluster collection status`"
			cs.Unmet = append(cs.Unmet, "no_guaranteed_copies")
			out.cs = cs
			return out
		}
		cm := n.clusters.collPtr.Load()
		if cm == nil {
			cs.State, cs.Detail = cluster.ReadyUnknown, "collection state unavailable"
			out.cs = cs
			return out
		}
		unit, pl = "item", cluster.NewPlacement(v, d.Policy, self)
		var root cid.CID
		root, groups, known = cm.FloorGroups(name, pl)
		known = known && root == target
		if r, ok := cm.Report(name); ok {
			out.evidence[self] = cluster.Evidence{Report: r, Received: out.now}
			ls := &apiv0.LocalSync{Target: r.Target, Mode: r.Mode, State: r.State, Complete: r.Complete, Owned: apiv0.Counts(r.Owned)}
			if r.Missing != nil {
				ls.Missing = apiv0.Counts(*r.Missing)
			}
			cs.Local = ls
		}
	} else {
		if m = n.replicaFor(c); m == nil {
			cs.State, cs.Detail = cluster.ReadyUnknown, "placement state unavailable"
			out.cs = cs
			return out
		}
		roots := []cid.CID{target}
		if live != nil {
			roots = append(roots, live.roots...)
		}
		groups, pl, known = m.Groups(name, roots...)
		if s, ok := m.Status(name); ok {
			out.evidence[self] = cluster.Evidence{Report: s.Reported, Received: out.now}
			ls := &apiv0.LocalSync{Target: s.Target.String(), Mode: s.Mode, State: s.State, Complete: s.Complete,
				Owned: apiv0.Counts(s.Owned), Missing: apiv0.Counts(s.Missing), Reason: s.Reason, Detail: s.Detail, Need: s.Need}
			if s.Observed.Defined() {
				ls.Observed = s.Observed.String()
			}
			cs.Local = ls
		}
		p := m.Protected(name)
		cs.Release = &apiv0.ReleaseState{Auto: c.Mode() != cluster.ModeLocal && n.settings().ReleaseAuto, Ready: p.Ready, Held: p.Held, Staged: p.Staged, Candidates: p.Retained}
	}
	key := c.ID.String() + "/" + name
	if live != nil {
		cs.Live, key = live.id, key+"#live"
	}
	cs.Epoch = pl.Epoch
	if c.Mode() != cluster.ModeLocal {
		if svc := n.clusters.svcPtr.Load(); svc != nil {
			for id, e := range svc.Evidence(name) {
				if id != self {
					out.evidence[id] = e
				}
			}
		}
	}
	n.reps.mu.Lock()
	mark, was := n.reps.ready[key]
	n.reps.mu.Unlock()
	in := cluster.ReadyInput{Target: target.String(), Policy: d.PolicyHash(), Epoch: pl.Epoch, Feasibility: pl.Feasible(), K: pl.K(),
		Groups: groups, OwnersKnown: known, Unit: unit, Evidence: out.evidence, Now: out.now, Fresh: out.fresh, WasReady: was && mark.target == target.String()}
	if live != nil {
		in.Live = live.id
	}
	if c.Mode() != cluster.ModeLocal {
		in.Members = v.IsMember
	}
	res := cluster.Ready(in)
	cs.State, cs.Required, cs.EvidenceAge = res.State, res.Required, res.EvidenceAge.Milliseconds()
	cs.Confirmed, cs.Extra = confirmations(res.Confirmed), confirmations(res.Extra)
	if known {
		cs.Copies = &apiv0.CopyCounts{Unit: res.Copies.Unit, Units: res.Copies.Units, Below: res.Copies.Below, Min: res.Copies.Min}
	}
	for _, mo := range res.Missing {
		cs.Missing = append(cs.Missing, apiv0.MissingOwner{Peer: peerStr(mo.Peer), Reason: mo.Reason})
	}
	cs.Unmet = append(cs.Unmet, res.Unmet...)
	if res.State == cluster.ReadyReady {
		mark = readyMark{target: target.String(), epoch: pl.Epoch, age: res.EvidenceAge, at: out.now}
		n.reps.mu.Lock()
		if n.reps.ready == nil {
			n.reps.ready = map[string]readyMark{}
		}
		n.reps.ready[key] = mark
		n.reps.mu.Unlock()
		was = true
	}
	if was && mark.target == target.String() {
		cs.LastReady = &apiv0.LastReady{Target: mark.target, Epoch: mark.epoch, EvidenceAgeMS: mark.age.Milliseconds(), At: mark.at}
	}
	// This node's own acquisition explains a syncing result.
	if l := cs.Local; l != nil && res.State == cluster.ReadySyncing && l.Target == cs.Target && (l.State == cluster.SyncPaused || l.State == cluster.SyncError) {
		cs.State, cs.Detail = l.State, l.Reason+": "+l.Detail
	}
	switch cs.State {
	case cluster.ReadyUnsatisfied:
		cs.Detail = "the policy cannot be met with the current members; adjust members, tags or the policy"
	case cluster.ReadyUnknown:
		if cs.Detail == "" {
			cs.Detail = "evidence is stale or unavailable; this is not data loss"
		}
	}
	n.contentAlerts(c, name, &cs)
	out.cs = cs
	return out
}

// headKind detects a head record's kind from held content.
func (n *Local) headKind(c cid.CID) (kind string, size int64, err error) {
	if c.Codec() == cid.Raw {
		info, ok, err := n.Store.Has(c)
		if err == nil && ok {
			size = int64(info.Size)
		}
		return "blob", size, nil
	}
	data, err := n.Store.Get(c)
	if err != nil {
		return "", 0, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is not held here, so its kind is unknown", c).WithAction("pass --kind tree|file|blob, or add the content first")
	}
	v, err := drisl.Decode(data)
	m, _ := v.(map[string]any)
	if err != nil || m == nil {
		return "", 0, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is not a tree header, file node or blob", c)
	}
	switch m["$type"] {
	case tree.TypeHeader:
		return "tree", 0, nil
	case tree.TypeMASL:
		return "file", 0, nil
	}
	return "", 0, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is not a tree header, file node or blob", c)
}

// DatasetUpdate writes a dataset's head. The result reports signed-state acknowledgment
// (metadata) and content readiness (content) separately (E10).
func (n *Local) DatasetUpdate(ctx context.Context, scope, name string, req apiv0.DatasetUpdateRequest) (*apiv0.Dataset, error) {
	c, err := n.scopeCluster(scope)
	if err != nil {
		return nil, err
	}
	d, ok := c.View().Dataset(name)
	if !ok {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "no dataset %q in the %s cluster", name, scope)
	}
	if d.Collection != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is a collection: its heads come from item pushes", name).
			WithAction("push items (`" + nu.Name + " cluster collection put`)")
	}
	if d.Source.Atrepo != "" {
		if req.Target != "" {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q archives %s: its heads are verified commits from the PDS", name, d.Source.Atrepo).
				WithAction("run `" + nu.Name + " dataset update " + name + "` without a target to fetch now")
		}
		n.refreshAtrepo(ctx, c, name, d)
		out := n.datasetInfo(c, name)
		if s := n.atrepoGet(name); s.err != "" {
			return out, apiv0.Errorf(apiv0.CodeUnavailable, "update of %s failed: %s", d.Source.Atrepo, s.err)
		}
		if hs := histFor(c, name, d); hs.headTID != "" {
			vi := n.versionInfo(ctx, hs, hs.headTID)
			out.Version = &vi
		}
		return out, nil
	}
	target, err := cid.Parse(req.Target)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "bad target %q: %v", req.Target, err)
	}
	kind, size, err := req.Kind, int64(0), error(nil)
	if kind == "" {
		if kind, size, err = n.headKind(target); err != nil {
			return nil, err
		}
	} else if kind != "tree" && kind != "file" && kind != "blob" {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "kind must be tree, file or blob")
	}
	return n.writeHead(ctx, c, name, d, target, kind, size, req.Note, "")
}

// writeHead writes a head record and, for a dataset with history, its version record in
// the same commit (restoredFrom names the version a rollback restores).
func (n *Local) writeHead(ctx context.Context, c *cluster.Cluster, name string, d *cluster.Dataset, target cid.CID, kind string, size int64, note, restoredFrom string) (*apiv0.Dataset, error) {
	return n.writeHeadImported(ctx, c, name, d, target, kind, size, note, restoredFrom, nil)
}

// writeHeadImported is writeHead whose version record also carries an import reference.
func (n *Local) writeHeadImported(ctx context.Context, c *cluster.Cluster, name string, d *cluster.Dataset, target cid.CID, kind string, size int64, note, restoredFrom string, imported *cluster.Imported) (*apiv0.Dataset, error) {
	ops := []cluster.Op{{Collection: cluster.NSHead, RKey: name, Value: &cluster.Head{Target: target.String(), Kind: kind, Size: size, Note: note}}}
	var tid string
	if d.Policy.History != nil {
		op, ver, t, err := n.versionOp(ctx, histFor(c, name, d), ops[0], target, kind, note, restoredFrom)
		if err != nil {
			return nil, writeErr(err)
		}
		ver.Imported = imported
		ops, tid = append(ops, op), t
	}
	w, err := c.Write(ops...)
	if err != nil {
		return nil, writeErr(err)
	}
	m := n.replicaFor(c)
	if m != nil && c.Mode() == cluster.ModeLocal {
		m.Reconcile() // the one-node case: content is usually already here
	}
	md := n.ack(ctx, c, w)
	out := n.datasetInfo(c, name)
	out.Metadata = md
	if tid != "" {
		vi := n.versionInfo(ctx, histFor(c, name, d), tid)
		out.Version = &vi
	}
	return out, nil
}

func waitError(code apiv0.Code, res *apiv0.WaitResult, format string, args ...any) error {
	e := apiv0.Errorf(code, format, args...).WithDetail("result", res)
	if code == apiv0.CodeTimeout {
		e = e.WithAction("see `unmet` and `missing` in the result for what is still missing; `" + nu.Name + " dataset status` shows the same fields")
	}
	return e
}

// DatasetWait waits for one condition on an exact target (docs/operations.md W1–W3).
func (n *Local) DatasetWait(ctx context.Context, scope, name string, req apiv0.DatasetWaitRequest) (*apiv0.WaitResult, error) {
	c, err := n.scopeCluster(scope)
	if err != nil {
		return nil, err
	}
	d, ok := c.View().Dataset(name)
	if !ok {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "no dataset %q in the %s cluster", name, scope)
	}
	target, err := cid.Parse(req.Target)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "a wait needs an exact target CID, not %q", req.Target).
			WithAction("names, \"latest\" and heads are never wait targets; pass the CID the head write returned")
	}
	cond := req.Condition
	if cond == "" {
		cond = apiv0.WaitReady
	}
	timeout := 10 * time.Minute
	switch cond {
	case apiv0.WaitMetadata:
		timeout = 30 * time.Second
	case apiv0.WaitReady:
	case apiv0.WaitHeld:
		if len(req.Holders) == 0 {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "a held wait needs at least one --holder")
		}
	case apiv0.WaitArchive:
		if d.Policy.History == nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "dataset %q keeps no version history; wait for ready instead", name)
		}
	default:
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "unknown condition %q", cond).WithAction("use metadata, held, ready or archive-ready")
	}
	// An archive-ready wait binds to the live set current when this node first sees the
	// target in it (W1: never retargeted by later versions). Until then (the version
	// record has not reached this member yet) it rebinds on every evaluation, so a wait
	// started right after another member's update does not bind to the previous live set.
	var live *liveBind
	bind := func() {
		if cond != apiv0.WaitArchive || (live != nil && slices.Contains(live.roots, target)) {
			return
		}
		if dd, ok := c.View().Dataset(name); ok {
			d = dd
		}
		hs := histFor(c, name, d)
		live = &liveBind{id: hs.ret.ID, roots: hs.liveRoots()}
	}
	bind()
	if req.Timeout != "" {
		if timeout, err = time.ParseDuration(req.Timeout); err != nil || timeout < 0 {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "timeout must be a duration such as 10m (0 waits indefinitely)")
		}
	}
	var holders []wire.PeerID
	for _, h := range req.Holders {
		id, err := wire.ParsePeerID(h)
		if err != nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "bad holder %q: %v", h, err)
		}
		holders = append(holders, id)
	}
	acks := max(req.Acks, 1)
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	for {
		bind()
		res, fail := n.waitEval(c, name, target, cond, holders, acks, req.Mode, live)
		switch {
		case res.Satisfied:
			return res, nil
		case fail != "":
			return res, waitError(apiv0.CodeFailed, res, "the %s condition for %s cannot be met without a state change: %s", cond, target, fail)
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-deadline:
			return res, waitError(apiv0.CodeTimeout, res, "timed out after %s waiting for %s of %s (%s)", timeout, cond, target, strings.Join(res.Unmet, "; "))
		case <-time.After(WaitPoll):
		}
	}
}

// waitEval is one evaluation of a wait (W6: the same evaluation as status). fail is
// non-empty when the condition cannot be met without a state change.
func (n *Local) waitEval(c *cluster.Cluster, name string, target cid.CID, cond string, holders []wire.PeerID, acks int, mode string, live *liveBind) (*apiv0.WaitResult, string) {
	v := c.View()
	ev := n.evalTarget(c, name, target, live)
	res := &apiv0.WaitResult{ContentState: ev.cs, Condition: cond}
	ts := target.String()
	observed := false
	var signer string
	for did, h := range v.Heads[name] {
		if !h.Deleted && h.Value != nil && h.Value.Target == ts {
			observed = true
			if signer == "" || did < signer {
				signer = did
			}
		}
	}
	if h, ok := v.Head(name); ok && h.Value.Target != ts {
		res.SupersededBy = h.Value.Target
	}
	if d, ok := v.Dataset(name); ok && d.Collection != nil {
		// A collection's target is its merged catalog root, which no single head names.
		observed, res.SupersededBy = false, ""
		if cm := n.clusters.collPtr.Load(); cm != nil {
			if root, ok := cm.MergedRoot(name); ok {
				observed = root == target
				if !observed {
					res.SupersededBy = root.String()
				}
			}
		}
	}
	if !observed {
		res.Unmet = append(res.Unmet, "target_not_observed: no head record of this dataset names it (yet)")
	}
	switch cond {
	case apiv0.WaitMetadata:
		if !observed {
			return res, ""
		}
		held, _ := c.Held(signer)
		mw := &apiv0.MetadataWait{Signer: signer, Rev: held.Rev, AckedBy: []string{}, Acks: acks}
		var acked []wire.PeerID
		if c.Mode() == cluster.ModeLocal {
			acked = []wire.PeerID{n.PeerID()}
		} else if svc := n.clusters.svcPtr.Load(); svc != nil {
			acked = svc.Acked(signer, held.Rev)
		}
		for _, id := range acked {
			mw.AckedBy = append(mw.AckedBy, peerStr(id))
		}
		res.Metadata = mw
		res.Satisfied = len(acked) >= acks
		if !res.Satisfied {
			res.Unmet = append(res.Unmet, fmt.Sprintf("metadata held by %d of %d storage member(s)", len(acked), acks))
		}
		return res, ""
	case apiv0.WaitHeld:
		res.Confirmed, res.Missing, res.Required = []apiv0.Confirmation{}, []apiv0.MissingOwner{}, len(holders)
		res.Mode = mode
		res.EvidenceAge = 0
		for _, id := range holders {
			if c.Mode() != cluster.ModeLocal && !v.IsMember(id) {
				return res, fmt.Sprintf("holder %s is not a member", peerStr(id))
			}
			e, ok := ev.evidence[id]
			age := ev.now.Sub(e.Received)
			reason := ""
			switch r := e.Report; {
			case !ok:
				reason = cluster.ReasonAbsent
			case age > ev.fresh:
				reason = cluster.ReasonStale
			case r.Target != ts:
				reason = cluster.ReasonAbsent
			case mode != "" && r.Mode != mode && (mode != cluster.ModeOwned || r.Mode != cluster.ModeFull): // full covers owned
				reason = cluster.ReasonEpoch
			case !r.Complete:
				reason = cluster.ReasonIncomplete
			}
			if reason != "" {
				res.Missing = append(res.Missing, apiv0.MissingOwner{Peer: peerStr(id), Reason: reason})
				res.Unmet = append(res.Unmet, fmt.Sprintf("holder %s: %s", peerStr(id), reason))
				continue
			}
			res.Confirmed = append(res.Confirmed, apiv0.Confirmation{Peer: peerStr(id), Seq: e.Report.Seq, AgeMS: age.Milliseconds()})
			res.EvidenceAge = max(res.EvidenceAge, age.Milliseconds())
		}
		res.Satisfied = len(res.Missing) == 0
		return res, ""
	}
	// ready, archive-ready
	res.Satisfied = res.State == cluster.ReadyReady
	if res.State == cluster.ReadyUnsatisfied {
		return res, strings.Join(res.Unmet, "; ")
	}
	return res, ""
}
