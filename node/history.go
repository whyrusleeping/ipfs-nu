package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/node/target"
	"github.com/whyrusleeping/nu/replica"
	"github.com/whyrusleeping/nu/store"
)

// Version history, retention, retrieval and rollback (docs/cluster.md "Version history";
// docs/operations.md "Retention and historical availability"; 0047).

// HistoryNode carries the version-history commands; Local and the HTTP client implement it.
type HistoryNode interface {
	DatasetVersions(ctx context.Context, scope, name string) (*apiv0.DatasetVersions, error)
	DatasetLabel(ctx context.Context, scope, name string, req apiv0.VersionLabelRequest) (*apiv0.VersionLabel, error)
	DatasetRollback(ctx context.Context, scope, name string, req apiv0.RollbackRequest) (*apiv0.Dataset, error)
	DatasetDiff(ctx context.Context, scope, name, from, to string) (*apiv0.DatasetDiff, error)
	DatasetPrune(ctx context.Context, scope, name string, req apiv0.PruneRequest) (*apiv0.PruneReport, error)
}

var _ HistoryNode = (*Local)(nil)

// histState is a dataset's merged history as this node sees it.
type histState struct {
	c       *cluster.Cluster
	name    string
	d       *cluster.Dataset
	hist    []cluster.VersionEntry
	ret     cluster.Retention
	head    string // head target ("" without a head)
	headTID string
}

func histFor(c *cluster.Cluster, name string, d *cluster.Dataset) *histState {
	v := c.View()
	hs := &histState{c: c, name: name, d: d, hist: v.VersionHistory(name)}
	if h, ok := v.Head(name); ok {
		hs.head = h.Value.Target
		hs.headTID = cluster.HeadVersion(hs.hist, hs.head)
	}
	hs.ret = cluster.Retain(name, hs.hist, d.Policy.History, hs.headTID)
	return hs
}

// history returns a dataset's history; datasets without history are invalid_argument.
func (n *Local) history(scope, name string) (*histState, error) {
	c, err := n.scopeCluster(scope)
	if err != nil {
		return nil, err
	}
	d, ok := c.View().Dataset(name)
	if !ok {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "no dataset %q in the %s cluster", name, scope)
	}
	if d.Policy.History == nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "dataset %q keeps no version history (a mirror follows current state)", name).
			WithAction("create archives with `" + nu.Name + " dataset create <ds> --archive`")
	}
	return histFor(c, name, d), nil
}

func (hs *histState) live(tid string) bool {
	_, ok := hs.ret.Live[tid]
	return ok
}

func (hs *histState) liveRoots() []cid.CID {
	var out []cid.CID
	for _, e := range hs.hist {
		if hs.live(e.TID) {
			if c, err := cid.Parse(e.Root); err == nil && !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
	}
	return out
}

func (hs *histState) selectVersion(sel string) (cluster.VersionEntry, error) {
	e, err := cluster.SelectVersion(hs.hist, sel)
	switch {
	case errors.Is(err, cluster.ErrAmbiguousVersion):
		return e, apiv0.Errorf(apiv0.CodeConflict, "%s: %v", hs.name, err).WithAction("select the version by its TID (see `" + nu.Name + " dataset versions`)")
	case err != nil:
		return e, apiv0.Errorf(apiv0.CodeNotFound, "%s: %v", hs.name, err).WithAction("list versions with `" + nu.Name + " dataset versions " + hs.name + "`; a time selects the newest version at or before it, never a later one")
	}
	return e, nil
}

func historyPolicy(h *cluster.History) *apiv0.HistoryPolicy {
	if h == nil {
		return nil
	}
	return &apiv0.HistoryPolicy{KeepLast: h.KeepLast, Hourly: h.Hourly, Daily: h.Daily, Weekly: h.Weekly, Monthly: h.Monthly, Yearly: h.Yearly, MaxBytes: h.MaxBytes}
}

// presetPolicy applies the archive/mirror presets of a create request.
func presetPolicy(req apiv0.DatasetCreateRequest, pol *cluster.Policy, src *cluster.Source, collection bool) error {
	if req.Archive && req.Mirror {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "--archive and --mirror exclude each other")
	}
	if (req.Archive || req.Mirror || req.History != nil) && collection {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "collections keep no version history; presets apply to ordinary datasets")
	}
	if req.Mirror && req.History != nil {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "a mirror keeps no history; use --archive for a retention schedule")
	}
	if h := req.History; h != nil {
		pol.History = &cluster.History{KeepLast: h.KeepLast, Hourly: h.Hourly, Daily: h.Daily, Weekly: h.Weekly, Monthly: h.Monthly, Yearly: h.Yearly, MaxBytes: h.MaxBytes}
	} else if req.Archive {
		pol.History = cluster.ArchiveHistory()
	}
	if p := req.SourcePath; p != "" {
		if !filepath.IsAbs(p) || len(p) > 1024 {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "the source path must be absolute (at most 1024 bytes)")
		}
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "the source path %s is not a readable directory", p)
		}
		src.Path = filepath.Clean(p)
	}
	return nil
}

// walkBlobs returns the held blobs reachable from root with their sizes.
func (n *Local) walkBlobs(ctx context.Context, root cid.CID) map[cid.Multihash]int64 {
	out := map[cid.Multihash]int64{}
	_, _ = n.Store.Walk(ctx, n.pinOf(root), func(c cid.CID, held bool) error {
		if c.Codec() == cid.Raw && held {
			if info, ok, err := n.Store.Has(c); err == nil && ok {
				out[c.Multihash()] = int64(info.Size)
			}
		}
		return nil
	})
	return out
}

// contentStats reads files and bytes from a held root (zero when not held).
func (n *Local) contentStats(root cid.CID, kind string) (files, bytes int64) {
	switch kind {
	case "blob":
		if info, ok, err := n.Store.Has(root); err == nil && ok {
			return 1, int64(info.Size)
		}
	case "file":
		if data, err := n.Store.Get(root); err == nil {
			if f, err := tree.DecodeFileNode(data); err == nil {
				return 1, int64(f.Nu.Size)
			}
		}
	case "tree":
		if data, err := n.Store.Get(root); err == nil {
			if h, err := tree.DecodeHeader(data); err == nil {
				return int64(h.Files), int64(h.Bytes)
			}
		}
	}
	return 0, 0
}

// versionOp builds the version record a head write on a dataset with history appends:
// identity fields from the held content, parent the current head's version, newBytes the
// held blobs absent from the parent's.
func (n *Local) versionOp(ctx context.Context, hs *histState, headOp cluster.Op, target cid.CID, kind, note, restoredFrom string) (cluster.Op, *cluster.Version, string, error) {
	tid := hs.c.NextTID()
	op := cluster.Op{Collection: cluster.NSVersion, RKey: cluster.VersionKey(hs.name, tid)}
	author, err := hs.c.Signer(headOp, op)
	if err != nil {
		return op, nil, "", err
	}
	ver := &cluster.Version{Root: target.String(), Kind: kind, Author: author, Parent: hs.headTID, RestoredFrom: restoredFrom, Note: note}
	ver.Files, ver.Bytes = n.contentStats(target, kind)
	blobs := n.walkBlobs(ctx, target)
	var parent map[cid.Multihash]int64
	if hs.headTID != "" {
		if pc, err := cid.Parse(hs.head); err == nil {
			parent = n.walkBlobs(ctx, pc)
		}
	}
	for mh, sz := range blobs {
		if _, ok := parent[mh]; !ok {
			ver.NewBytes += sz
		}
	}
	op.Value = ver
	return op, ver, tid, nil
}

// fullyHeld reports whether everything reachable from root is held here.
func (n *Local) fullyHeld(ctx context.Context, root string, cache map[string]bool) bool {
	if v, ok := cache[root]; ok {
		return v
	}
	c, err := cid.Parse(root)
	ok := err == nil && n.requireComplete(ctx, c) == nil
	cache[root] = ok
	return ok
}

// versionInfos evaluates every version's availability (docs/cluster.md "Availability")
// and, with a head, archive-wide readiness over the live set (E8).
func (n *Local) versionInfos(ctx context.Context, hs *histState) ([]apiv0.VersionInfo, *apiv0.ContentState) {
	m := n.replicaFor(hs.c)
	var prot replica.Protection
	var status replica.Status
	var hasStatus bool
	if m != nil {
		if hs.c.Mode() == cluster.ModeLocal {
			m.Reconcile()
		}
		prot = m.Protected(hs.name)
		status, hasStatus = m.Status(hs.name)
	}
	var archive *apiv0.ContentState
	var incomplete map[string]bool // live versions a fresh owner report lists incomplete
	if hs.head != "" {
		if hc, err := cid.Parse(hs.head); err == nil {
			ev := n.evalTarget(hs.c, hs.name, hc, &liveBind{id: hs.ret.ID, roots: hs.liveRoots()})
			archive = &ev.cs
			incomplete = map[string]bool{}
			for _, e := range ev.evidence {
				if ev.now.Sub(e.Received) <= ev.fresh && e.Report.Live == hs.ret.ID {
					for _, t := range e.Report.Incomplete {
						incomplete[t] = true
					}
				}
			}
			if ev.cs.State == cluster.ReadyReady && m != nil {
				var tids []string
				for tid := range hs.ret.Live {
					tids = append(tids, tid)
				}
				m.Establish(hs.name, tids)
				prot = m.Protected(hs.name)
			}
		}
	}
	local := hs.c.Mode() == cluster.ModeLocal
	held := map[string]bool{}
	out := make([]apiv0.VersionInfo, 0, len(hs.hist))
	for _, e := range hs.hist {
		vi := apiv0.VersionInfo{TID: e.TID, Time: e.Time.UTC(), Root: e.Root, Kind: e.Kind, Parent: e.Parent, Author: e.Author, Files: e.Files,
			Bytes: e.Bytes, NewBytes: e.NewBytes, RestoredFrom: e.RestoredFrom, Label: e.Label, Note: e.Note, AnnotatedBy: e.AnnotatedBy,
			Head: e.TID == hs.headTID, Live: hs.live(e.TID), Reasons: hs.ret.Live[e.TID]}
		if e.Observed != nil {
			vi.Rev, vi.Commit = e.Observed.Rev, e.Observed.Commit
		}
		for _, x := range e.Conflicts {
			vi.Conflicts = append(vi.Conflicts, apiv0.VersionConflict{Signer: x.Signer, Root: x.Root, Author: x.Author})
		}
		established := slices.Contains(prot.Complete, e.TID)
		here := n.fullyHeld(ctx, e.Root, held)
		switch {
		case here:
			vi.Availability = apiv0.AvailComplete
			if !local {
				vi.Detail = "complete on this node"
			}
		case slices.Contains(prot.Pruned, e.TID):
			vi.Availability, vi.Detail = apiv0.AvailPruned, "metadata only: released by prune; label it with --reacquire to fetch it again"
		case local && established:
			vi.Availability, vi.Detail = apiv0.AvailDegraded, "was complete here, content is now missing"
		case !local && established && incomplete[e.TID]:
			vi.Availability, vi.Detail = apiv0.AvailDegraded, "a fresh owner report lists it incomplete"
		case !local && established && archive != nil && archive.State == cluster.ReadyUnknown:
			vi.Availability, vi.Detail = apiv0.AvailUnknown, "owner evidence is stale; this is not data loss"
		case !local && established:
			vi.Availability, vi.Detail = apiv0.AvailComplete, "confirmed by an archive-ready evaluation"
		case !vi.Live:
			vi.Availability, vi.Detail = apiv0.AvailSkipped, "outside the live set and never completed here"
		case hasStatus && status.State == cluster.SyncPaused:
			vi.Availability, vi.Detail = apiv0.AvailFetching, "paused: "+status.Reason+": "+status.Detail
		case hasStatus && (status.LiveVersions != nil || status.Target.String() == e.Root) && m != nil && n.Fetcher != nil && !isNoFetcher(n.Fetcher):
			vi.Availability = apiv0.AvailFetching
		default:
			vi.Availability, vi.Detail = apiv0.AvailObserved, "not acquired here yet (no provider: add the content or connect a node that holds it)"
		}
		out = append(out, vi)
	}
	return out, archive
}

func isNoFetcher(f Fetcher) bool {
	_, ok := f.(NoFetcher)
	return ok
}

func (n *Local) DatasetVersions(ctx context.Context, scope, name string) (*apiv0.DatasetVersions, error) {
	hs, err := n.history(scope, name)
	if err != nil {
		return nil, err
	}
	vs, archive := n.versionInfos(ctx, hs)
	return &apiv0.DatasetVersions{Name: name, History: historyPolicy(hs.d.Policy.History), Live: hs.ret.ID, LiveBytes: hs.ret.Bytes,
		Excess: hs.ret.Excess, Archive: archive, Versions: vs}, nil
}

// versionInfo returns one version's info.
func (n *Local) versionInfo(ctx context.Context, hs *histState, tid string) apiv0.VersionInfo {
	vs, _ := n.versionInfos(ctx, hs)
	for _, v := range vs {
		if v.TID == tid {
			return v
		}
	}
	return apiv0.VersionInfo{TID: tid, Availability: apiv0.AvailObserved}
}

// unavailable is the error for a version that is not retained-complete: it names the
// availability and never falls back to another version.
func unavailable(hs *histState, vi apiv0.VersionInfo) error {
	return apiv0.Errorf(apiv0.CodeUnavailable, "version %s of %s is %s, not retained-complete: %s", vi.TID, hs.name, vi.Availability, vi.Detail).
		WithDetail("version", vi.TID).WithDetail("availability", vi.Availability).
		WithAction("reacquire it with `" + nu.Name + " dataset label " + hs.name + " " + vi.TID + " <label> --reacquire` and wait for it, or select another version explicitly")
}

func (n *Local) DatasetLabel(ctx context.Context, scope, name string, req apiv0.VersionLabelRequest) (*apiv0.VersionLabel, error) {
	hs, err := n.history(scope, name)
	if err != nil {
		return nil, err
	}
	e, err := hs.selectVersion(req.Version)
	if err != nil {
		return nil, err
	}
	label := req.Label
	if req.Remove {
		label = ""
	} else {
		if !cluster.ValidLabel(label) {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "label %q must match [a-z][a-z0-9._-]{0,63} and not look like a TID or a time", label)
		}
		for _, o := range hs.hist {
			if o.Label == label && o.TID != e.TID {
				return nil, apiv0.Errorf(apiv0.CodeConflict, "label %q is already on version %s", label, o.TID).
					WithAction("remove it there first (`" + nu.Name + " dataset label " + name + " " + o.TID + " --rm`)")
			}
		}
		if vi := n.versionInfo(ctx, hs, e.TID); vi.Availability != apiv0.AvailComplete && !req.Reacquire {
			return nil, apiv0.Errorf(apiv0.CodeUnavailable, "version %s is %s: labeling it would not make its content available", e.TID, vi.Availability).
				WithDetail("availability", vi.Availability).WithAction("pass --reacquire to make it an acquisition obligation; it stays " + vi.Availability + " until its content is complete")
		}
	}
	note := e.Note
	if req.Note != "" {
		note = req.Note
	}
	w, err := hs.c.Write(cluster.Op{Collection: cluster.NSVersion, RKey: cluster.VersionKey(name, e.TID), Value: e.Annotated(label, note)})
	if err != nil {
		return nil, writeErr(err)
	}
	if m := n.replicaFor(hs.c); m != nil && hs.c.Mode() == cluster.ModeLocal {
		m.Reconcile()
	}
	hs = histFor(hs.c, name, hs.d)
	return &apiv0.VersionLabel{Version: n.versionInfo(ctx, hs, e.TID), Metadata: n.ack(ctx, hs.c, w)}, nil
}

func (n *Local) DatasetRollback(ctx context.Context, scope, name string, req apiv0.RollbackRequest) (*apiv0.Dataset, error) {
	hs, err := n.history(scope, name)
	if err != nil {
		return nil, err
	}
	e, err := hs.selectVersion(req.Version)
	if err != nil {
		return nil, err
	}
	if e.Root == hs.head {
		return nil, apiv0.Errorf(apiv0.CodeConflict, "version %s (%s) is already the head", e.TID, e.Root)
	}
	if vi := n.versionInfo(ctx, hs, e.TID); vi.Availability != apiv0.AvailComplete {
		return nil, unavailable(hs, vi)
	}
	target, err := cid.Parse(e.Root)
	if err != nil {
		return nil, err
	}
	note := req.Note
	if note == "" {
		note = "rollback to " + e.TID
	}
	return n.writeHead(ctx, hs.c, name, hs.d, target, e.Kind, 0, note, e.TID)
}

// resolveDataset resolves a dataset target to a root: the head, or a version that must be
// retained-complete (no fallback to another version).
func (n *Local) resolveDataset(ctx context.Context, pt target.Target) (cid.CID, error) {
	var c *cluster.Cluster
	var d *cluster.Dataset
	scope := ""
	for _, s := range []string{ScopeLocal, ScopeCluster} {
		if s == ScopeLocal && !cluster.Exists(n.clusterDir("local")) {
			continue
		}
		sc, err := n.scopeCluster(s)
		if err != nil {
			continue
		}
		if dd, ok := sc.View().Dataset(pt.Dataset); ok {
			c, d, scope = sc, dd, s
			break
		}
	}
	if c == nil {
		return cid.CID{}, apiv0.Errorf(apiv0.CodeNotFound, "no dataset %q on this node", pt.Dataset).
			WithAction("list datasets with `" + nu.Name + " dataset ls` or `" + nu.Name + " cluster dataset ls`")
	}
	if d.Source.Name != "" && (pt.Version == "" || pt.Version == "held") {
		// A follow serves its held view (docs/follow.md "Lifecycle"): the last fully held
		// target, which may be older than the head being acquired.
		if m := n.replicaFor(c); m != nil {
			if held := m.Protected(pt.Dataset).Held; held != "" {
				return cid.Parse(held)
			}
		}
		if pt.Version == "held" {
			return cid.CID{}, apiv0.Errorf(apiv0.CodeUnavailable, "follow %q holds no complete view yet", pt.Dataset).
				WithAction("see `" + nu.Name + " follow ls`")
		}
	}
	if pt.Version == "" {
		h, ok := c.View().Head(pt.Dataset)
		if !ok {
			return cid.CID{}, apiv0.Errorf(apiv0.CodeNotFound, "dataset %q has no head yet", pt.Dataset)
		}
		return cid.Parse(h.Value.Target)
	}
	hs, err := n.history(scope, pt.Dataset)
	if err != nil {
		return cid.CID{}, err
	}
	e, err := hs.selectVersion(pt.Version)
	if err != nil {
		return cid.CID{}, err
	}
	if vi := n.versionInfo(ctx, hs, e.TID); vi.Availability != apiv0.AvailComplete {
		return cid.CID{}, unavailable(hs, vi)
	}
	return cid.Parse(e.Root)
}

// ResolveDataset is the gateway's /c/ lookup: the root of a dataset's head or version.
func (n *Local) ResolveDataset(ctx context.Context, name, version string) (string, error) {
	c, err := n.resolveDataset(ctx, target.Target{Kind: target.KindDataset, Dataset: name, Version: version})
	if err != nil {
		return "", err
	}
	return c.String(), nil
}

const maxDiffPaths = 10000

func (n *Local) DatasetDiff(ctx context.Context, scope, name, from, to string) (*apiv0.DatasetDiff, error) {
	hs, err := n.history(scope, name)
	if err != nil {
		return nil, err
	}
	a, err := hs.selectVersion(from)
	if err != nil {
		return nil, err
	}
	b, err := hs.selectVersion(to)
	if err != nil {
		return nil, err
	}
	out := &apiv0.DatasetDiff{From: a.TID, To: b.TID, Added: []string{}, Removed: []string{}, Changed: []string{}}
	if a.Root == b.Root {
		return out, nil
	}
	if a.Kind == cluster.KindAtrepo || b.Kind == cluster.KindAtrepo {
		if a.Kind != b.Kind {
			out.Changed = append(out.Changed, "")
			return out, nil
		}
		return out, n.atrepoDiff(a, b, hs.d, out)
	}
	ra, err1 := n.treeRoot(a)
	rb, err2 := n.treeRoot(b)
	if err := errors.Join(err1, err2); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeUnavailable, "cannot diff: %v", err).WithAction("both versions' structure must be held here")
	}
	if !ra.Defined() || !rb.Defined() { // a file or blob on either side
		out.Changed = append(out.Changed, "")
		return out, nil
	}
	err = n.diffDirs(ra, rb, "", out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// treeRoot returns a tree version's root directory (zero for files and blobs).
func (n *Local) treeRoot(e cluster.VersionEntry) (cid.CID, error) {
	if e.Kind != "tree" {
		return cid.CID{}, nil
	}
	c, err := cid.Parse(e.Root)
	if err != nil {
		return cid.CID{}, err
	}
	data, err := n.Store.Get(c)
	if err != nil {
		return cid.CID{}, fmt.Errorf("version %s: %w", e.TID, err)
	}
	h, err := tree.DecodeHeader(data)
	if err != nil {
		return cid.CID{}, err
	}
	return h.Root, nil
}

func (n *Local) diffDirs(a, b cid.CID, prefix string, out *apiv0.DatasetDiff) error {
	full := func() bool { return len(out.Added)+len(out.Removed)+len(out.Changed) >= maxDiffPaths }
	return mst.Diff(tree.Dir(n.Store, a), tree.Dir(n.Store, b), func(ch mst.Change[tree.Entry]) error {
		if full() {
			out.Truncated = true
			return nil
		}
		p := prefix + ch.Key
		switch {
		case ch.Old == nil:
			out.Added = append(out.Added, dirSlash(p, ch.New))
		case ch.New == nil:
			out.Removed = append(out.Removed, dirSlash(p, ch.Old))
		case ch.Old.Kind == tree.KindDir && ch.New.Kind == tree.KindDir:
			return n.diffDirs(ch.Old.CID, ch.New.CID, p+"/", out)
		default:
			out.Changed = append(out.Changed, p)
		}
		return nil
	})
}

func dirSlash(p string, e *tree.Entry) string {
	if e.Kind == tree.KindDir {
		return p + "/"
	}
	return p
}

func (n *Local) DatasetPrune(ctx context.Context, scope, name string, req apiv0.PruneRequest) (*apiv0.PruneReport, error) {
	hs, err := n.history(scope, name)
	if err != nil {
		return nil, err
	}
	m := n.replicaFor(hs.c)
	if m == nil {
		return nil, apiv0.Errorf(apiv0.CodeUnavailable, "placement state unavailable")
	}
	if hs.c.Mode() == cluster.ModeLocal {
		m.Reconcile()
	}
	if !req.DryRun && hs.c.Mode() != cluster.ModeLocal {
		// S1–S3: versions outside the live set leave only once owners confirm the live set
		// k times in the current, settled epoch.
		rr, ok := m.ReleaseReport(name, true)
		if !ok || !rr.HeadConfirmed || len(rr.Deferred) > 0 {
			var why []string
			why = append(why, rr.Deferred...)
			for _, s := range rr.HeadShortfall {
				why = append(why, "owner "+s.Peer+": "+s.Reason)
			}
			return nil, apiv0.Errorf(apiv0.CodeFailed, "the live set is not confirmed by its owners in a settled epoch yet: %s", strings.Join(why, "; ")).
				WithDetail("release", rr).WithAction("wait until `" + nu.Name + " cluster dataset gc " + name + " --dry-run` shows head_confirmed and nothing deferred")
		}
	}
	prot := m.Protected(name)
	liveRoots := map[string]bool{}
	for _, r := range hs.liveRoots() {
		liveRoots[r.String()] = true
	}
	labels := map[string]string{}
	for _, e := range hs.hist {
		labels[e.TID] = e.Label
	}
	rep := &apiv0.PruneReport{DryRun: req.DryRun, Candidates: []apiv0.PruneCandidate{}}
	cands := map[string]string{}
	candRoots := map[cid.CID]bool{}
	for tid, root := range prot.Versions {
		if hs.live(tid) || liveRoots[root] || root == hs.head || root == prot.Ready || root == prot.Held || slices.Contains(prot.Staged, root) {
			continue
		}
		c, err := cid.Parse(root)
		if err != nil {
			continue
		}
		cands[tid] = root
		candRoots[c] = true
		rep.Candidates = append(rep.Candidates, apiv0.PruneCandidate{TID: tid, Root: root, Label: labels[tid]})
	}
	slices.SortFunc(rep.Candidates, func(a, b apiv0.PruneCandidate) int { return strings.Compare(a.TID, b.TID) })
	if hs.ret.Excess > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("protected versions (labels, head) exceed max_bytes by %d bytes; they are never pruned", hs.ret.Excess))
	}
	if err := n.pruneImpact(ctx, name, candRoots, rep); err != nil {
		return nil, err
	}
	if req.DryRun || len(cands) == 0 {
		return rep, nil
	}
	pruned, err := m.Prune(name, cands, hs.head, liveRoots)
	rep.Pruned = pruned
	if err != nil {
		return rep, err
	}
	rep.Warnings = append(rep.Warnings, "released content is removed by the store's next GC; the versions' metadata stays (availability: pruned)")
	return rep, nil
}

// pruneImpact computes the physical bytes releasing the candidate roots' pins would free
// (objects no other pin reaches) and those they share with other pins.
func (n *Local) pruneImpact(ctx context.Context, name string, cands map[cid.CID]bool, rep *apiv0.PruneReport) error {
	if len(cands) == 0 {
		return nil
	}
	pins, err := n.Store.Pins()
	if err != nil {
		return err
	}
	kept := map[cid.Multihash]bool{}
	isCand := func(p store.Pin) bool {
		return cands[p.CID] && p.Source == store.SourceCluster && p.Name == "dataset/"+name
	}
	for _, p := range pins {
		if isCand(p) {
			continue
		}
		if _, err := n.Store.Walk(ctx, p, func(c cid.CID, held bool) error {
			kept[c.Multihash()] = true
			return nil
		}); err != nil {
			return err
		}
	}
	seen := map[cid.Multihash]bool{}
	for c := range cands {
		if _, err := n.Store.Walk(ctx, n.pinOf(c), func(c cid.CID, held bool) error {
			mh := c.Multihash()
			if seen[mh] || !held {
				return nil
			}
			seen[mh] = true
			info, ok, err := n.Store.Has(c)
			if err != nil || !ok {
				return err
			}
			if kept[mh] {
				rep.SharedKept += int64(info.Physical)
			} else {
				rep.FreeBytes += int64(info.Physical)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// establishArchive records the live versions of a dataset with history as established
// complete when the archive is ready over its live set (placement passes call it).
func (n *Local) establishArchive(c *cluster.Cluster, name string) {
	d, ok := c.View().Dataset(name)
	if !ok || d.Policy.History == nil {
		return
	}
	hs := histFor(c, name, d)
	hc, err := cid.Parse(hs.head)
	if err != nil {
		return
	}
	if n.evalTarget(c, name, hc, &liveBind{id: hs.ret.ID, roots: hs.liveRoots()}).cs.State != cluster.ReadyReady {
		return
	}
	var tids []string
	for tid := range hs.ret.Live {
		tids = append(tids, tid)
	}
	if m := n.replicaFor(c); m != nil {
		m.Establish(name, tids)
	}
}

// liveBind binds an evaluation to a live set (archive-wide readiness, E8).
type liveBind struct {
	id    string
	roots []cid.CID
}
