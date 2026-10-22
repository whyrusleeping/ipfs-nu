package node

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/atrepo"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/node/content"
	"github.com/whyrusleeping/nu/store"
)

// atproto accounts as archive datasets (0054; docs/dasl.md D21 "atproto accounts as
// archive datasets"). A dataset with source {atrepo: did} is updated by the admin
// daemon from the account's PDS: getRepo?since=<held rev> applied with ApplySince (a
// full export when the chain breaks), each verified commit written as the head with its
// rev and, under the archive preset, as a version with `observed` provenance. The
// account's status is polled with getRepoStatus; while it is inactive, updates stop and
// the status is recorded on the head so every member applies the redistribution rule.
// Updates are polling only: Nu does not consume Jetstream/firehose for dataset ingest.

// atrepoState is in-memory bookkeeping per atrepo dataset on the fetching node.
type atrepoState struct {
	refreshed                time.Time
	err, mode, pdsRev        string
	handle                   string
	blobs, oversized, failed int
}

func (n *Local) atrepoNote(ds string, fn func(s *atrepoState)) {
	n.names.mu.Lock()
	defer n.names.mu.Unlock()
	if n.names.atrepos == nil {
		n.names.atrepos = map[string]*atrepoState{}
	}
	s := n.names.atrepos[ds]
	if s == nil {
		s = &atrepoState{}
		n.names.atrepos[ds] = s
	}
	fn(s)
}

func (n *Local) atrepoGet(ds string) atrepoState {
	n.names.mu.Lock()
	defer n.names.mu.Unlock()
	if s := n.names.atrepos[ds]; s != nil {
		return *s
	}
	return atrepoState{}
}

// atrepoSource fills an atproto-account source from a create request: the account is
// resolved to its DID now (the dataset binds to the DID), the scope and the override are
// recorded, and Own is set when this node is logged in to the account.
func (n *Local) atrepoSource(ctx context.Context, req apiv0.DatasetCreateRequest, src *cluster.Source) error {
	u, err := atproto.ParseURI(req.SourceAt)
	if err != nil {
		return err
	}
	if u.Collection != "" || u.Rev != "" {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "an atproto source is a whole account: at://<handle-or-did>")
	}
	if !req.Archive && !req.Mirror && req.History == nil {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "an atproto source needs a preset").
			WithAction("add --archive (every verified commit is a retained version) or --mirror (current state only)")
	}
	if req.SourcePath != "" {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "a dataset has one source: a directory or an atproto account")
	}
	if len(req.NSIDs) > cluster.MaxNSIDs {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "at most %d nsids", cluster.MaxNSIDs)
	}
	for _, id := range req.NSIDs {
		if id == "" || strings.ContainsAny(id, "/ \t") || len(id) > 317 {
			return apiv0.Errorf(apiv0.CodeInvalidArgument, "bad nsid %q", id)
		}
	}
	did, err := n.resolveAt(ctx, u)
	if err != nil {
		return err
	}
	src.Local = false
	src.Atrepo, src.NSIDs, src.Blobs, src.PublishHistory = did, slices.Clone(req.NSIDs), req.Blobs, req.PublishHistory
	if ss, err := n.sessionStore(); err == nil {
		if s, err := ss.Load(did); err == nil && s != nil {
			src.Own = true
		}
	}
	return nil
}

// atrepoDatasets lists the atrepo datasets of a cluster (name → record).
func atrepoDatasets(c *cluster.Cluster) map[string]*cluster.Dataset {
	out := map[string]*cluster.Dataset{}
	for ds, d := range c.View().Datasets {
		if !d.Deleted && d.Value != nil && d.Value.Source.Atrepo != "" {
			out[ds] = d.Value
		}
	}
	return out
}

// atrepoPin is the cluster pin of an atrepo dataset's commit: atrepo+blobs when blobs
// are selected (so GC keeps them), atrepo otherwise.
func atrepoProfile(d *cluster.Dataset) string {
	if d.Source.Blobs {
		return "atrepo+blobs"
	}
	return "atrepo"
}

// inNSIDs reports whether a record path <collection>/<rkey> is in the source's scope.
func inNSIDs(d *cluster.Dataset, path string) bool {
	if len(d.Source.NSIDs) == 0 {
		return true
	}
	coll, _, _ := strings.Cut(path, "/")
	return slices.Contains(d.Source.NSIDs, coll)
}

// heldAtrepo returns the dataset's held head commit (its repo, rev) when it is an atrepo
// head held here.
func (n *Local) heldAtrepo(c *cluster.Cluster, ds string) (*atrepo.Repo, cid.CID, string, bool) {
	h, ok := c.View().Head(ds)
	if !ok || h.Value.Kind != cluster.KindAtrepo || h.Value.Observed == nil {
		return nil, cid.CID{}, "", false
	}
	root, err := cid.Parse(h.Value.Target)
	if err != nil {
		return nil, cid.CID{}, "", false
	}
	r, err := atrepo.Open(n.Store, root)
	if err != nil {
		return nil, root, h.Value.Observed.Rev, false
	}
	return r, root, h.Value.Observed.Rev, true
}

// refreshAtrepo runs one update of an atrepo dataset: account status, then a since-diff
// (or a full export) when the PDS has a newer rev, blobs per policy, head and version.
func (n *Local) refreshAtrepo(ctx context.Context, c *cluster.Cluster, ds string, d *cluster.Dataset) {
	now := time.Now()
	fail := func(mode string, err error) {
		n.atrepoNote(ds, func(s *atrepoState) { s.refreshed, s.err, s.mode = now, err.Error(), mode })
	}
	cl, err := n.atproto()
	if err != nil {
		fail("", err)
		return
	}
	id, err := cl.Resolve(ctx, d.Source.Atrepo)
	if err != nil {
		fail("resolve", err)
		return
	}
	st, err := cl.RepoStatus(ctx, id)
	if err != nil {
		fail("status", err)
		return
	}
	n.atrepoNote(ds, func(s *atrepoState) { s.handle, s.pdsRev = id.Handle, st.Rev })
	head, hasHead := c.View().Head(ds)
	if !st.Active {
		// Updates stop; the status goes on the head so every member applies the
		// redistribution rule. History is kept.
		if !hasHead {
			fail("status", apiv0.Errorf(apiv0.CodeUnavailable, "account %s is %s; nothing was archived", id.DID, st.Status))
			return
		}
		if head.Value.Status != st.Status {
			h := *head.Value
			h.Status = st.Status
			if _, err := c.Write(cluster.Op{Collection: cluster.NSHead, RKey: ds, Value: &h}); err != nil && !errors.Is(err, cluster.ErrNoKey) {
				fail("status", err)
				return
			}
		}
		n.atrepoNote(ds, func(s *atrepoState) { s.refreshed, s.err, s.mode = now, "", "status" })
		return
	}
	if hasHead && head.Value.Observed != nil && st.Rev != "" && head.Value.Observed.Rev >= st.Rev {
		if head.Value.Status != "" { // reactivated: current state may be served again
			h := *head.Value
			h.Status = ""
			if _, err := c.Write(cluster.Op{Collection: cluster.NSHead, RKey: ds, Value: &h}); err != nil && !errors.Is(err, cluster.ErrNoKey) {
				fail("status", err)
				return
			}
		}
		n.atrepoNote(ds, func(s *atrepoState) { s.refreshed, s.err, s.mode = now, "", "status" })
		return
	}
	mode, commit, rev, ops, err := n.fetchAtrepo(ctx, cl, id, c, ds, d)
	if err != nil {
		fail(mode, err)
		return
	}
	if hasHead && head.Value.Observed != nil && head.Value.Observed.Rev >= rev {
		n.atrepoNote(ds, func(s *atrepoState) { s.refreshed, s.err, s.mode = now, "", mode })
		return
	}
	if len(d.Source.NSIDs) > 0 && ops != nil && !slices.ContainsFunc(ops, func(op atrepo.Op) bool { return inNSIDs(d, op.Path) }) {
		// Nothing in scope changed: no new version (the held rev stays the diff base).
		n.atrepoNote(ds, func(s *atrepoState) { s.refreshed, s.err, s.mode = now, "", mode })
		return
	}
	if d.Source.Blobs {
		n.fetchAtrepoBlobs(ctx, cl, id, ds, d, commit, ops)
	}
	note := "observed rev " + rev
	if mode == "full" {
		note += " (full export)"
	}
	if err := n.writeAtrepoHead(ctx, c, ds, d, commit, rev, ops, note); err != nil {
		if errors.Is(err, cluster.ErrNoKey) {
			err = errors.New("") // storage-only members follow the admins' heads
		}
		fail(mode, err)
		return
	}
	n.atrepoNote(ds, func(s *atrepoState) { s.refreshed, s.err, s.mode = now, "", mode })
}

// fetchAtrepo brings the account's current commit into the store: a since-diff over the
// held head when one is held (mode "since"), else or on a chain break a full export
// ("full"). ops are the record operations of a since-diff (nil for a full export). A
// signature failure retries once with a freshly resolved identity (key rotation).
func (n *Local) fetchAtrepo(ctx context.Context, cl *atproto.Client, id *atproto.Identity, c *cluster.Cluster, ds string, d *cluster.Dataset) (mode string, commit cid.CID, rev string, ops []atrepo.Op, err error) {
	ps := &putStore{st: n.Store}
	if held, _, heldRev, ok := n.heldAtrepo(c, ds); ok {
		a, err := cl.GetRepoSince(ctx, id, heldRev, ps, held.Head.Data)
		if ps.err != nil {
			return "since", cid.CID{}, "", nil, apiErr(ps.err)
		}
		if err == nil {
			return "since", a.CommitCID, a.Commit.Rev, a.Ops, nil
		}
		if ctx.Err() != nil {
			return "since", cid.CID{}, "", nil, err
		}
		// Chain break, migration or an unknown rev: fall back to a full export.
	}
	snap, err := cl.GetRepo(ctx, id, ps, nil)
	if ps.err != nil {
		return "full", cid.CID{}, "", nil, apiErr(ps.err)
	}
	if err != nil && errors.Is(err, atrepo.ErrSignature) && time.Since(id.Resolved) > time.Second {
		cl.Forget(id.DID)
		if id2, rerr := cl.Resolve(ctx, id.DID); rerr == nil {
			*id = *id2
			snap, err = cl.GetRepo(ctx, id, ps, nil)
		}
	}
	if err != nil {
		return "full", cid.CID{}, "", nil, err
	}
	return "full", snap.Commit, snap.Rev, nil, nil
}

// fetchAtrepoBlobs fetches the blobs the new revision's in-scope records reference (the
// created/updated records of a diff, every record after a full export) that are not
// held; oversized and failed blobs are counted, never fatal.
func (n *Local) fetchAtrepoBlobs(ctx context.Context, cl *atproto.Client, id *atproto.Identity, ds string, d *cluster.Dataset, commit cid.CID, ops []atrepo.Op) {
	var want []cid.CID
	seen := map[cid.CID]bool{}
	add := func(rc cid.CID, path string) {
		if !inNSIDs(d, path) {
			return
		}
		data, err := n.Store.Get(rc)
		if err != nil {
			return
		}
		for _, b := range atrepo.RecordBlobs(data) {
			if seen[b] {
				continue
			}
			seen[b] = true
			if _, ok, _ := n.Store.Has(b); !ok {
				want = append(want, b)
			}
		}
	}
	if ops != nil {
		for _, op := range ops {
			if op.CID.Defined() {
				add(op.CID, op.Path)
			}
		}
	} else if r, err := atrepo.Open(n.Store, commit); err == nil {
		_ = r.Tree.Walk("", func(k string, v cid.CID) (bool, error) { add(v, k); return true, nil })
	}
	limit := cl.Limits().MaxBlobs
	for i, b := range want {
		if ctx.Err() != nil {
			return
		}
		if i >= limit {
			n.atrepoNote(ds, func(s *atrepoState) { s.failed += len(want) - i })
			return
		}
		data, err := cl.GetBlob(ctx, id, b)
		switch {
		case errors.Is(err, atproto.ErrOversized):
			n.atrepoNote(ds, func(s *atrepoState) { s.oversized++ })
		case err != nil:
			n.atrepoNote(ds, func(s *atrepoState) { s.failed++ })
		default:
			if n.Store.Put(b, data) == nil {
				n.atrepoNote(ds, func(s *atrepoState) { s.blobs++ })
			}
		}
	}
}

// writeAtrepoHead writes the verified commit as the head (rev and commit in observed)
// and, with history, its version: files are the in-scope records, bytes the held bytes
// of structure, records and blobs, newBytes those absent from the parent (O(changes)
// from the diff's ops when available).
func (n *Local) writeAtrepoHead(ctx context.Context, c *cluster.Cluster, ds string, d *cluster.Dataset, commit cid.CID, rev string, ops []atrepo.Op, note string) error {
	obs := &cluster.Observed{Rev: rev, Commit: commit.String()}
	ops2 := []cluster.Op{{Collection: cluster.NSHead, RKey: ds, Value: &cluster.Head{Target: commit.String(), Kind: cluster.KindAtrepo, Note: note, Observed: obs}}}
	if d.Policy.History != nil {
		hs := histFor(c, ds, d)
		op, ver, _, err := n.versionOp(ctx, hs, ops2[0], commit, cluster.KindAtrepo, note, "")
		if err != nil {
			return err
		}
		ver.Observed = obs
		n.atrepoVersionStats(ctx, hs, d, commit, ops, ver)
		ops2 = append(ops2, op)
	}
	if _, err := c.Write(ops2...); err != nil {
		return err
	}
	if m := n.replicaFor(c); m != nil {
		if c.Mode() == cluster.ModeLocal {
			m.Reconcile()
		} else {
			m.Kick()
		}
	}
	return nil
}

// atrepoVersionStats fills Files (in-scope records), Bytes (held bytes of structure,
// records and blobs under the profile) and NewBytes of an atrepo version: O(changes)
// from the diff (new MST nodes, created/updated records and their blobs) when ops are
// known, else the blocks absent from the parent's walk.
func (n *Local) atrepoVersionStats(ctx context.Context, hs *histState, d *cluster.Dataset, commit cid.CID, ops []atrepo.Op, ver *cluster.Version) {
	size := func(c cid.CID) int64 {
		if info, ok, err := n.Store.Has(c); err == nil && ok {
			return int64(info.Size)
		}
		return 0
	}
	pin := func(c cid.CID) store.Pin {
		return store.Pin{CID: c, Mode: store.Recursive, Profile: atrepoProfile(d), Source: store.SourceExplicit}
	}
	var parent cid.CID
	for _, e := range hs.hist {
		if e.TID == hs.headTID {
			parent, _ = cid.Parse(e.Root)
		}
	}
	if r, err := atrepo.Open(n.Store, commit); err == nil {
		_ = r.Tree.Walk("", func(k string, _ cid.CID) (bool, error) {
			if inNSIDs(d, k) {
				ver.Files++
			}
			return true, nil
		})
	}
	var prev map[cid.Multihash]bool
	if parent.Defined() && ops == nil {
		prev = map[cid.Multihash]bool{}
		_, _ = n.Store.Walk(ctx, pin(parent), func(c cid.CID, held bool) error {
			prev[c.Multihash()] = true
			return nil
		})
	}
	_, _ = n.Store.Walk(ctx, pin(commit), func(c cid.CID, held bool) error {
		if held {
			sz := size(c)
			ver.Bytes += sz
			if ops == nil && !prev[c.Multihash()] {
				ver.NewBytes += sz
			}
		}
		return nil
	})
	if ops == nil || !parent.Defined() {
		return
	}
	seen := map[cid.CID]bool{}
	add := func(c cid.CID) {
		if !seen[c] {
			seen[c] = true
			ver.NewBytes += size(c)
		}
	}
	add(commit)
	if nodes, err := atrepo.NewNodes(n.Store, parent, commit); err == nil {
		for _, nd := range nodes {
			add(nd)
		}
	}
	for _, op := range ops {
		if !op.CID.Defined() {
			continue
		}
		add(op.CID)
		if d.Source.Blobs && inNSIDs(d, op.Path) {
			if data, err := n.Store.Get(op.CID); err == nil {
				for _, b := range atrepo.RecordBlobs(data) {
					add(b)
				}
			}
		}
	}
}

// refreshAtrepos updates every atrepo dataset of both scopes (startup, poll backstop,
// on demand); FollowLoop calls it with the name follows.
func (n *Local) refreshAtrepos(ctx context.Context) {
	for _, scope := range []string{ScopeLocal, ScopeCluster} {
		if scope == ScopeLocal && !cluster.Exists(n.clusterDir("local")) {
			continue
		}
		c, err := n.scopeCluster(scope)
		if err != nil {
			continue
		}
		for ds, d := range atrepoDatasets(c) {
			n.refreshAtrepo(ctx, c, ds, d)
		}
	}
}

// atrepoInfo is the dataset's atrepo source state for status.
func (n *Local) atrepoInfo(c *cluster.Cluster, ds string, d *cluster.Dataset) *apiv0.AtrepoSource {
	s := n.atrepoGet(ds)
	out := &apiv0.AtrepoSource{DID: d.Source.Atrepo, Handle: s.handle, NSIDs: d.Source.NSIDs, Blobs: d.Source.Blobs, Own: d.Source.Own,
		PublishHistory: d.Source.PublishHistory, Status: "active", PDSRev: s.pdsRev, Refreshed: s.refreshed, Error: s.err, Mode: s.mode,
		BlobsFetched: s.blobs, BlobsOversized: s.oversized, BlobsFailed: s.failed}
	if h, ok := c.View().Head(ds); ok {
		if h.Value.Observed != nil {
			out.Rev = h.Value.Observed.Rev
		}
		if h.Value.Status != "" {
			out.Status = h.Value.Status
		}
	}
	out.Redistribution = redistribution(d, out.Status)
	return out
}

// Redistribution rules (D21): own accounts and the explicit override are unrestricted;
// a third-party account's current verified state may leave the cluster while the
// account is active; its retained history and an inactive account's state may not.
const (
	RedistUnrestricted = "unrestricted"
	RedistCurrentOnly  = "current-only"
	RedistPrivate      = "private"
)

func redistribution(d *cluster.Dataset, status string) string {
	switch {
	case d.Source.Own || d.Source.PublishHistory:
		return RedistUnrestricted
	case status != "" && status != "active":
		return RedistPrivate
	}
	return RedistCurrentOnly
}

// redistributable reports whether root may be given to parties outside this node and
// its cluster (share links, public marks): an atrepo dataset's rule applies to its
// head and every retained version; other roots are unrestricted.
func (n *Local) redistributable(root cid.CID) error {
	for _, scope := range []string{ScopeLocal, ScopeCluster} {
		if scope == ScopeLocal && !cluster.Exists(n.clusterDir("local")) {
			continue
		}
		c, err := n.scopeCluster(scope)
		if err != nil {
			continue
		}
		v := c.View()
		for ds, d := range atrepoDatasets(c) {
			status := ""
			head := ""
			if h, ok := v.Head(ds); ok {
				head, status = h.Value.Target, h.Value.Status
			}
			rule := redistribution(d, status)
			isHead := head == root.String()
			inHistory := slices.ContainsFunc(v.VersionHistory(ds), func(e cluster.VersionEntry) bool { return e.Root == root.String() })
			if !isHead && !inHistory {
				continue
			}
			switch {
			case rule == RedistUnrestricted:
				return nil
			case rule == RedistPrivate:
				return apiv0.Errorf(apiv0.CodeForbidden, "%s is the archive of %s, whose account is %s: it stays private (local or cluster-only)", root, d.Source.Atrepo, status).
					WithAction("atproto mirrors stop serving deactivated, taken-down and deleted accounts; the history stays for members")
			case isHead:
				return nil
			}
			return apiv0.Errorf(apiv0.CodeForbidden, "%s is retained history of %s, a third-party account archived as dataset %q: history is private by default", root, d.Source.Atrepo, ds).
				WithAction("only the current verified state leaves the cluster; to publish history, recreate the dataset with --publish-history (recorded on the dataset) or archive an account you are logged in to (`" + nu.Name + " login`)")
		}
	}
	return nil
}

// atrepoDiff is the record-level diff of two atrepo versions: paths <collection>/<rkey>
// created, deleted and updated between the revs, in scope.
func (n *Local) atrepoDiff(a, b cluster.VersionEntry, d *cluster.Dataset, out *apiv0.DatasetDiff) error {
	ca, err1 := cid.Parse(a.Root)
	cb, err2 := cid.Parse(b.Root)
	if err := errors.Join(err1, err2); err != nil {
		return err
	}
	ra, err1 := atrepo.Open(n.Store, ca)
	rb, err2 := atrepo.Open(n.Store, cb)
	if err := errors.Join(err1, err2); err != nil {
		return apiv0.Errorf(apiv0.CodeUnavailable, "cannot diff: %v", err).WithAction("both versions' structure must be held here")
	}
	ops, err := atrepo.Ops(n.Store, ra.Head.Data, rb.Head.Data)
	if err != nil {
		return apiv0.Errorf(apiv0.CodeUnavailable, "cannot diff: %v", err)
	}
	for _, op := range ops {
		if !inNSIDs(d, op.Path) {
			continue
		}
		if len(out.Added)+len(out.Removed)+len(out.Changed) >= maxDiffPaths {
			out.Truncated = true
			break
		}
		switch op.Action() {
		case "create":
			out.Added = append(out.Added, op.Path)
		case "delete":
			out.Removed = append(out.Removed, op.Path)
		default:
			out.Changed = append(out.Changed, op.Path)
		}
	}
	return nil
}

// atrepoRoot opens root as an atproto commit when it is one (dataset browsing).
func (n *Local) atrepoRoot(root cid.CID) (*atrepo.Repo, bool) {
	if root.Codec() != cid.DRISL {
		return nil, false
	}
	data, err := n.Store.Get(root)
	if err != nil {
		return nil, false
	}
	if _, err := atrepo.DecodeCommit(data); err != nil {
		return nil, false
	}
	r, err := atrepo.Open(n.Store, root)
	return r, err == nil
}

// atrepoListing lists an atrepo dataset root at path "" (collections),
// "<collection>" (records) or "<collection>/<rkey>" (one record), in scope.
func (n *Local) atrepoListing(r *atrepo.Repo, root cid.CID, d *cluster.Dataset, p, label string) (*apiv0.Listing, error) {
	s := &content.Snapshot{DID: r.DID, Rev: r.Head.Rev, Commit: root, Repo: r}
	l := &apiv0.Listing{Target: label, Entries: []apiv0.Entry{}}
	coll, rkey, isRecord := strings.Cut(p, "/")
	switch {
	case p == "":
		cols, err := n.content.Collections(s)
		if err != nil {
			return nil, err
		}
		for _, c := range cols {
			if d == nil || inNSIDs(d, c.Name+"/") {
				l.Entries = append(l.Entries, apiv0.Entry{Name: c.Name, CID: root.String(), Type: "collection", Size: int64(c.Count)})
			}
		}
	case !isRecord:
		if d != nil && !inNSIDs(d, coll+"/") {
			return nil, apiv0.Errorf(apiv0.CodeNotFound, "collection %s is outside the dataset's nsids", coll)
		}
		recs, err := n.content.Records(s, coll)
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			l.Entries = append(l.Entries, apiv0.Entry{Name: rec.RKey, CID: rec.CID.String(), Type: "record", Size: rec.Size})
		}
	default:
		if strings.Contains(rkey, "/") {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "records have no sub-paths").WithAction("use <dataset>[@<version>]/<collection>/<rkey>")
		}
		if d != nil && !inNSIDs(d, p) {
			return nil, apiv0.Errorf(apiv0.CodeNotFound, "collection %s is outside the dataset's nsids", coll)
		}
		rc, data, err := n.content.Record(s, coll, rkey)
		if err != nil {
			return nil, err
		}
		l.Entries = append(l.Entries, apiv0.Entry{Name: rkey, CID: rc.String(), Type: "record", Size: int64(len(data))})
	}
	return l, nil
}

// atrepoRecord returns a record of an atrepo dataset root as JSON (cat).
func (n *Local) atrepoRecord(r *atrepo.Repo, root cid.CID, d *cluster.Dataset, p string) ([]byte, error) {
	coll, rkey, ok := strings.Cut(p, "/")
	if !ok || rkey == "" || strings.Contains(rkey, "/") {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is a collection of an atproto archive, not a record", p).
			WithAction("list it with `ls`, or cat <dataset>[@<version>]/<collection>/<rkey>")
	}
	if d != nil && !inNSIDs(d, p) {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "collection %s is outside the dataset's nsids", coll)
	}
	s := &content.Snapshot{DID: r.DID, Rev: r.Head.Rev, Commit: root, Repo: r}
	_, data, err := n.content.Record(s, coll, rkey)
	if err != nil {
		return nil, err
	}
	js, err := content.RecordJSON(data)
	if err != nil {
		return nil, err
	}
	return append(js, '\n'), nil
}

// datasetOf finds the dataset record owning a target (any scope).
func (n *Local) datasetOf(name string) *cluster.Dataset {
	for _, scope := range []string{ScopeLocal, ScopeCluster} {
		if scope == ScopeLocal && !cluster.Exists(n.clusterDir("local")) {
			continue
		}
		if c, err := n.scopeCluster(scope); err == nil {
			if d, ok := c.View().Dataset(name); ok {
				return d
			}
		}
	}
	return nil
}
