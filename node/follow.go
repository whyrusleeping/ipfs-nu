package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/collection"
	"github.com/whyrusleeping/nu/follow"
	"github.com/whyrusleeping/nu/names"
	"github.com/whyrusleeping/nu/node/target"
	"github.com/whyrusleeping/nu/store"
)

// Following (docs/follow.md): a follow is a mirror-preset dataset with a name source.
// Admin daemons resolve the name (poll backstop, on-demand refresh and the publisher's
// own writes) and write each newer verified target as the head with its proof's rev;
// the placement manager acquires content and reports completion like any dataset. The
// three roots are: observed (newest verified proof, the name cache), syncing (the head
// being acquired) and held (the last fully held target, served by the gateway).

// FollowPoll is the default poll backstop (docs/follow.md: `follow.poll`, 1 h jittered).
var FollowPoll = time.Hour

// followState is in-memory bookkeeping per follow dataset.
type followState struct {
	refreshed time.Time
	err       string
	// ended records a verified deletion: the last head and its rev stay for the user.
	ended    *apiv0.FollowRoot
	endedRev string
	// paused is a quota/scope pause reason ("" when none).
	paused string
}

func (n *Local) followNote(ds string, fn func(f *followState)) {
	n.names.mu.Lock()
	defer n.names.mu.Unlock()
	if n.names.follows == nil {
		n.names.follows = map[string]*followState{}
	}
	f := n.names.follows[ds]
	if f == nil {
		f = &followState{}
		n.names.follows[ds] = f
	}
	fn(f)
}

func (n *Local) followGet(ds string) followState {
	n.names.mu.Lock()
	defer n.names.mu.Unlock()
	if f := n.names.follows[ds]; f != nil {
		return *f
	}
	return followState{}
}

// parseNameSource splits at://<did>/io.ipfs.nu.name/<name>.
func parseNameSource(uri string) (did, name string, ok bool) {
	rest, ok := strings.CutPrefix(uri, "at://")
	if !ok {
		return "", "", false
	}
	did, rest, ok = strings.Cut(rest, "/"+names.CollectionName+"/")
	return did, rest, ok && did != "" && rest != ""
}

func (n *Local) Follow(ctx context.Context, req apiv0.FollowRequest) (*apiv0.Follow, error) {
	pt, err := target.Parse(req.Target)
	if err != nil {
		return nil, err
	}
	if pt.Kind != target.KindName || pt.Path != "" {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a public name", req.Target).
			WithAction("follow <handle-or-did>/<name> (DID-wide follows arrive later)")
	}
	if req.Lazy || req.Path != "" {
		return nil, apiv0.Errorf(apiv0.CodeNotSupported, "scoped (--path) and lazy follows are not supported yet").
			WithAction("follow the whole name; scoped and lazy modes arrive with the follow syncer work")
	}
	if req.MaxSize < 0 {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "max_size must not be negative")
	}
	var rules []collection.Rule
	if len(req.Select) > 0 {
		if err := json.Unmarshal(req.Select, &rules); err != nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "bad selection rules: %v", err).
				WithAction("rules are a list of flat rule objects, see docs/collections.md \"Selection\"")
		}
		if req.All || req.Replicas != 0 {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "--select holds items by rules on this node; --all and --replicas do not apply")
		}
		if len(rules) == 0 {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "--select needs at least one rule").
				WithAction("a rule with no predicates (`[[rule]]` alone) selects every item")
		}
		for i := range rules {
			if rules[i].Rescue != nil {
				return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "rule %d: %v", i+1, collection.ErrRescueFollow).
					WithAction("rescue needs a cluster holdings index; follow with plain selection rules")
			}
		}
	}
	scope := req.Scope
	if scope == "" {
		scope = ScopeLocal
	}
	c, err := n.scopeCluster(scope)
	if err != nil {
		return nil, err
	}
	// Resolve now: the follow binds to the DID, and the first head is written at once.
	r, err := n.resolver()
	if err != nil {
		return nil, err
	}
	res, err := r.Resolve(ctx, pt.Authority, pt.Name, names.Options{})
	var deleted *names.ErrDeleted
	if err != nil && !errors.As(err, &deleted) {
		return nil, err
	}
	did := pt.Authority
	if res != nil {
		did = res.DID
	} else if deleted != nil {
		did = deleted.DID
	}
	uri := "at://" + did + "/" + names.CollectionName + "/" + pt.Name
	ds := req.Dataset
	if ds == "" {
		handle := ""
		if res != nil {
			handle = res.Handle
		}
		ds = followDatasetName(pt.Name, handle)
	}
	if !cluster.ValidName(ds) {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "dataset name %q must match [a-z0-9-]{1,64}", ds)
	}
	for _, other := range []string{ScopeLocal, ScopeCluster} {
		oc, err := n.scopeCluster(other)
		if err != nil {
			continue
		}
		for name, d := range oc.View().Datasets {
			if !d.Deleted && d.Value != nil && d.Value.Source.Name == uri {
				if rules != nil && collection.IsFollow(d.Value) && (req.Dataset == "" || req.Dataset == name) {
					// The same name followed by selection again: the rules (and the
					// budget) are replaced.
					return n.selectRules(ctx, other, oc, name, d.Value, rules, req.MaxSize)
				}
				return nil, apiv0.Errorf(apiv0.CodeConflict, "%s is already followed as dataset %q (%s)", req.Target, name, other).
					WithAction("see `" + nu.Name + " follow ls`")
			}
		}
	}
	if _, ok := c.View().Dataset(ds); ok {
		return nil, apiv0.Errorf(apiv0.CodeConflict, "dataset %q already exists", ds).WithAction("pass --dataset <other-name>")
	}
	pol := cluster.Policy{Replicas: req.Replicas, All: req.All}
	if pol.Replicas == 0 && !pol.All && rules == nil {
		pol.Replicas = 2
		if scope == ScopeLocal {
			pol.Replicas = 1
		}
	}
	if pol.All {
		pol.Replicas = 0
	}
	src := cluster.Source{Name: uri}
	if req.MaxSize > 0 || req.NoAnnounce || rules != nil {
		src.Follow = &cluster.Follow{MaxSize: req.MaxSize, NoAnnounce: req.NoAnnounce, Select: rules != nil}
	}
	note := "follow of " + pt.Authority + "/" + pt.Name
	rec := &cluster.Dataset{Source: src, Policy: pol, Note: note}
	if rules != nil {
		// A select follow is a collection whose single writer is the followed name (D22
		// "Following collections"); its facets come from the published header.
		rec.Collection = &cluster.CollectionPolicy{}
	}
	if _, err := c.Write(cluster.Op{Collection: cluster.NSDataset, RKey: ds, Value: rec}); err != nil {
		if errors.Is(err, cluster.ErrNoKey) {
			return nil, writeErr(err)
		}
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "cannot write the dataset record: %v", err)
	}
	if rules != nil {
		cm, _ := n.collectionsOf(c)
		if cm == nil {
			return nil, apiv0.Errorf(apiv0.CodeFailed, "cannot open the collection state for the follow")
		}
		if _, err := cm.SetRules(ds, rules, false); err != nil {
			_, _ = c.Write(cluster.Op{Collection: cluster.NSDataset, RKey: ds, Delete: true})
			return nil, err
		}
	}
	d, _ := c.View().Dataset(ds)
	n.applyResolution(ctx, scope, c, ds, d, res, deleted)
	n.names.wake()
	return n.followInfo(ctx, scope, c, ds), nil
}

// selectRules replaces a select follow's rules and re-evaluates the selection; held
// items that the new rules keep are not fetched again.
func (n *Local) selectRules(ctx context.Context, scope string, c *cluster.Cluster, ds string, d *cluster.Dataset, rules []collection.Rule, maxSize int64) (*apiv0.Follow, error) {
	cm, started := n.collectionsOf(c)
	if cm == nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "cannot open the collection state for the follow")
	}
	if _, err := cm.SetRules(ds, rules, false); err != nil {
		return nil, err
	}
	if maxSize != d.Source.Follow.MaxSize {
		upd := *d
		fo := *d.Source.Follow
		fo.MaxSize = maxSize
		upd.Source.Follow = &fo
		if _, err := c.Write(cluster.Op{Collection: cluster.NSDataset, RKey: ds, Value: &upd}); err != nil {
			return nil, writeErr(err)
		}
	}
	n.syncFollowCollection(ctx, cm, started)
	return n.followInfo(ctx, scope, c, ds), nil
}

// syncFollowCollection moves a select follow along: a started manager is kicked, one
// without loops (no daemon) runs a synchronous pass.
func (n *Local) syncFollowCollection(ctx context.Context, cm *collection.Manager, started bool) {
	if started {
		cm.Kick()
		return
	}
	_ = cm.Sync(ctx)
}

// applyResolution writes a newer verified target as the head (D19 version ordering: only
// strictly newer revs; a same-record proof refreshes nothing) or records a deletion.
func (n *Local) applyResolution(ctx context.Context, scope string, c *cluster.Cluster, ds string, d *cluster.Dataset, res *names.Resolution, deleted *names.ErrDeleted) {
	now := time.Now()
	if deleted != nil {
		n.followNote(ds, func(f *followState) {
			f.refreshed, f.err = now, ""
			if f.ended == nil {
				f.ended = &apiv0.FollowRoot{}
				if h, ok := c.View().Head(ds); ok {
					f.ended.Target = h.Value.Target
					if h.Value.Observed != nil {
						f.ended.Rev = h.Value.Observed.Rev
					}
				}
			}
			f.endedRev = deleted.Rev
		})
		return
	}
	for _, b := range res.Proof {
		_ = n.Store.Put(b.CID, b.Data)
	}
	if h, ok := c.View().Head(ds); ok && h.Value.Observed != nil && h.Value.Observed.Rev >= res.Rev && h.Value.Target == res.Target.String() {
		n.followNote(ds, func(f *followState) { f.refreshed, f.err, f.ended, f.paused = now, "", nil, "" })
		return
	}
	if f := d.Source.Follow; f != nil && f.MaxSize > 0 && res.Size > f.MaxSize && !f.Select {
		n.followNote(ds, func(f *followState) {
			f.refreshed, f.err, f.ended = now, "", nil
			f.paused = fmt.Sprintf("quota: the publisher claims %d bytes, over the follow's max_size %d", res.Size, d.Source.Follow.MaxSize)
		})
		return
	}
	if _, err := n.writeHeadObserved(ctx, c, ds, d, res); err != nil {
		msg := err.Error()
		if errors.Is(err, cluster.ErrNoKey) {
			msg = "" // storage-only members follow the admins' heads
		}
		n.followNote(ds, func(f *followState) { f.refreshed, f.err, f.ended = now, msg, nil })
		return
	}
	n.followNote(ds, func(f *followState) { f.refreshed, f.err, f.ended, f.paused = now, "", nil, "" })
}

// writeHeadObserved writes a head carrying the proof's rev and commit.
func (n *Local) writeHeadObserved(ctx context.Context, c *cluster.Cluster, ds string, d *cluster.Dataset, res *names.Resolution) (*apiv0.Dataset, error) {
	ops := []cluster.Op{{Collection: cluster.NSHead, RKey: ds, Value: &cluster.Head{Target: res.Target.String(), Kind: res.Kind, Size: res.Size,
		Note: res.Note, Observed: &cluster.Observed{Rev: res.Rev, Commit: res.Commit.String()}}}}
	if d.Policy.History != nil {
		op, _, _, err := n.versionOp(ctx, histFor(c, ds, d), ops[0], res.Target, res.Kind, "observed rev "+res.Rev, "")
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	if _, err := c.Write(ops...); err != nil {
		return nil, err
	}
	if collection.IsFollow(d) {
		if cm, started := n.collectionsOf(c); cm != nil {
			n.syncFollowCollection(ctx, cm, started)
		}
		return n.datasetInfo(c, ds), nil
	}
	if m := n.replicaFor(c); m != nil {
		if c.Mode() == cluster.ModeLocal {
			m.Reconcile() // the one-node case: content is usually already here
		} else {
			m.Kick()
		}
	}
	return n.datasetInfo(c, ds), nil
}

// followInfo builds a follow's status from its dataset and the name cache.
func (n *Local) followInfo(ctx context.Context, scope string, c *cluster.Cluster, ds string) *apiv0.Follow {
	v := c.View()
	d, _ := v.Dataset(ds)
	did, name, _ := parseNameSource(d.Source.Name)
	info := n.datasetInfo(c, ds)
	fs := n.followGet(ds)
	f := &apiv0.Follow{Dataset: ds, Scope: scope, URI: d.Source.Name, DID: did, Name: name, Mode: d.Source.Follow.Mode(), Content: info.Content,
		Refreshed: fs.refreshed, Error: fs.err}
	now := time.Now()
	if r, err := n.resolver(); err == nil {
		if res, ok := r.Cached(did, name); ok {
			f.Handle = res.Handle
			f.Observed = &apiv0.FollowRoot{Target: res.Target.String(), Rev: res.Rev, Age: res.Age(now).Round(time.Second).String()}
		}
	}
	var headRev string
	if h, ok := v.Head(ds); ok {
		if h.Value.Observed != nil {
			headRev = h.Value.Observed.Rev
		}
		if l := info.Content.Local; l != nil && !l.Complete {
			f.Syncing = &apiv0.FollowRoot{Target: l.Target, Rev: headRev}
		}
	}
	if rel := info.Content.Release; rel != nil && rel.Held != "" {
		f.Held = &apiv0.FollowRoot{Target: rel.Held}
		if h, ok := v.Head(ds); ok && h.Value.Target == rel.Held {
			f.Held.Rev = headRev
		}
	}
	if collection.IsFollow(d) {
		// A select follow holds its catalog whole and items by rules: held is the head
		// whose catalog was synced, syncing a head not yet synced.
		if cm, _ := n.collectionsOf(c); cm != nil {
			if sel, err := cm.FollowSummary(ds); err == nil {
				f.Selection = sel
				f.Content.Detail = fmt.Sprintf("%d items in the catalog, %d selected (%d held, %d fetching, %d paused)", sel.Items, sel.Selected, sel.Held, sel.Fetching, sel.Paused)
				f.Content.State = "selecting"
				if sel.CatalogSynced && sel.Fetching == 0 {
					f.Content.State = "ready"
				}
			}
			if synced, ok := cm.SyncedHead(ds); ok {
				f.Held = &apiv0.FollowRoot{Target: synced.String()}
				if h, ok := v.Head(ds); ok && h.Value.Target == synced.String() {
					f.Held.Rev = headRev
				} else if ok {
					f.Syncing = &apiv0.FollowRoot{Target: h.Value.Target, Rev: headRev}
				}
			} else if h, ok := v.Head(ds); ok {
				f.Syncing = &apiv0.FollowRoot{Target: h.Value.Target, Rev: headRev}
			}
		}
	}
	switch {
	case fs.ended != nil:
		f.State, f.Ended = "ended", fs.ended
		f.Detail = "the publisher deleted the name (proven absent at rev " + fs.endedRev + "); the last target stays pinned until you unfollow"
		if fs.ended.Target != "" {
			f.Orphans = []string{fs.ended.Target}
		}
	case fs.paused != "":
		f.State, f.Detail = "paused", fs.paused
		f.Blockers = []string{fs.paused}
	case info.Content.Local != nil && (info.Content.Local.State == cluster.SyncPaused || info.Content.Local.State == cluster.SyncError):
		f.State, f.Detail = info.Content.Local.State, info.Content.Local.Reason+": "+info.Content.Local.Detail
		f.Blockers = []string{info.Content.Local.Reason}
	case fs.err != "":
		f.State, f.Detail = "error", fs.err
	case f.Syncing != nil && f.Held != nil:
		f.State, f.Detail = "lagging", "serving "+f.Held.Target+" while acquiring "+f.Syncing.Target
	case f.Syncing != nil:
		f.State = "syncing"
	case f.Held != nil:
		f.State = "held"
	default:
		f.State, f.Detail = "observed", "no head yet"
	}
	if f.Observed != nil && f.Held != nil && f.Observed.Target != f.Held.Target && f.State == "held" {
		f.State, f.Detail = "lagging", "a newer target "+f.Observed.Target+" is observed but not held"
	}
	return f
}

// followList lists every follow of both scopes.
func (n *Local) followList(ctx context.Context) []apiv0.Follow {
	var out []apiv0.Follow
	for _, scope := range []string{ScopeLocal, ScopeCluster} {
		if scope == ScopeLocal && !cluster.Exists(n.clusterDir("local")) {
			continue
		}
		c, err := n.scopeCluster(scope)
		if err != nil {
			continue
		}
		for ds, d := range c.View().Datasets {
			if !d.Deleted && d.Value != nil && d.Value.Source.Name != "" {
				out = append(out, *n.followInfo(ctx, scope, c, ds))
			}
		}
	}
	slices.SortFunc(out, func(a, b apiv0.Follow) int { return strings.Compare(a.Dataset, b.Dataset) })
	if out == nil {
		out = []apiv0.Follow{}
	}
	return out
}

func (n *Local) Follows(ctx context.Context) ([]apiv0.Follow, error) { return n.followList(ctx), nil }

// findFollow locates a follow by dataset name, or by <handle-or-did>/<name>.
func (n *Local) findFollow(ctx context.Context, key string) (string, *cluster.Cluster, string, error) {
	uri := ""
	if pt, err := target.Parse(key); err == nil && pt.Kind == target.KindName {
		did := pt.Authority
		if !strings.HasPrefix(did, "did:") {
			if c, err := n.atproto(); err == nil {
				if id, err := c.Resolve(ctx, did); err == nil {
					did = id.DID
				}
			}
		}
		uri = "at://" + did + "/" + names.CollectionName + "/" + pt.Name
	}
	for _, scope := range []string{ScopeLocal, ScopeCluster} {
		c, err := n.scopeCluster(scope)
		if err != nil {
			continue
		}
		for ds, d := range c.View().Datasets {
			if !d.Deleted && d.Value != nil && d.Value.Source.Name != "" && (ds == key || d.Value.Source.Name == uri) {
				return scope, c, ds, nil
			}
		}
	}
	return "", nil, "", apiv0.Errorf(apiv0.CodeNotFound, "no follow %q", key).WithAction("list follows with `" + nu.Name + " follow ls`")
}

// Unfollow deletes the follow's dataset record and releases its pins only; the content
// stays for GC unless another pin reaches it.
func (n *Local) Unfollow(ctx context.Context, key string) (*apiv0.Follow, error) {
	scope, c, ds, err := n.findFollow(ctx, key)
	if err != nil {
		return nil, err
	}
	f := n.followInfo(ctx, scope, c, ds)
	d, _ := c.View().Dataset(ds)
	if _, err := c.Write(cluster.Op{Collection: cluster.NSDataset, RKey: ds, Delete: true}); err != nil {
		return nil, writeErr(err)
	}
	if m := n.replicaFor(c); m != nil {
		m.Reconcile()
	}
	var released []string
	if collection.IsFollow(d) {
		if cm, _ := n.collectionsOf(c); cm != nil {
			released, _ = cm.Forget(ds)
		}
	}
	if released == nil {
		released = []string{}
	}
	pins, err := n.Store.Pins()
	if err == nil {
		for _, p := range pins {
			if p.Source == store.SourceCluster && p.Name == "dataset/"+ds {
				if _, err := n.Store.Unpin(p); err == nil {
					released = append(released, p.CID.String())
				}
			}
		}
		f.Orphans = released
	}
	f.State, f.Detail = "unfollowed", "the follow's pins were released; other pins of the same content stay"
	n.names.mu.Lock()
	delete(n.names.follows, ds)
	n.names.mu.Unlock()
	n.names.wake()
	return f, nil
}

// refreshFollow re-resolves one follow from its PDS (Fresh) and applies the answer.
func (n *Local) refreshFollow(ctx context.Context, scope string, c *cluster.Cluster, ds string, d *cluster.Dataset) {
	did, name, ok := parseNameSource(d.Source.Name)
	if !ok {
		return
	}
	r, err := n.resolver()
	if err != nil {
		return
	}
	res, err := r.Resolve(ctx, did, name, names.Options{Fresh: true})
	if err != nil {
		var del *names.ErrDeleted
		if errors.As(err, &del) {
			n.applyResolution(ctx, scope, c, ds, d, nil, del)
			return
		}
		n.followNote(ds, func(f *followState) { f.refreshed, f.err = time.Now(), err.Error() })
		return
	}
	n.applyResolution(ctx, scope, c, ds, d, res, nil)
}

func (n *Local) FollowRefresh(ctx context.Context, req apiv0.FollowRefreshRequest) ([]apiv0.Follow, error) {
	if req.Dataset != "" {
		scope, c, ds, err := n.findFollow(ctx, req.Dataset)
		if err != nil {
			return nil, err
		}
		d, _ := c.View().Dataset(ds)
		n.refreshFollow(ctx, scope, c, ds, d)
		return []apiv0.Follow{*n.followInfo(ctx, scope, c, ds)}, nil
	}
	n.refreshFollows(ctx)
	return n.followList(ctx), nil
}

// refreshFollows re-resolves every follow and updates every atrepo dataset (startup,
// poll backstop, on demand).
func (n *Local) refreshFollows(ctx context.Context) {
	n.names.mu.Lock()
	n.names.polled = time.Now()
	n.names.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		n.refreshAtrepos(ctx)
	}()
	for _, scope := range []string{ScopeLocal, ScopeCluster} {
		if scope == ScopeLocal && !cluster.Exists(n.clusterDir("local")) {
			continue
		}
		c, err := n.scopeCluster(scope)
		if err != nil {
			continue
		}
		for ds, d := range c.View().Datasets {
			if d.Deleted || d.Value == nil || d.Value.Source.Name == "" {
				continue
			}
			wg.Add(1)
			go func(ds string, d *cluster.Dataset) {
				defer wg.Done()
				n.refreshFollow(ctx, scope, c, ds, d)
			}(ds, d.Value)
		}
	}
	wg.Wait()
}

// followedDIDs lists the DIDs of every follow (the Jetstream filter).
func (n *Local) followedDIDs(ctx context.Context) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range n.followList(ctx) {
		if !seen[f.DID] {
			seen[f.DID] = true
			out = append(out, f.DID)
		}
	}
	return out
}

// Jetstream is the optional hint source (config follow.jetstream); the daemon sets it
// before FollowLoop. Events only trigger verified refreshes of followed names.
func (n *Local) SetJetstream(j *follow.Jetstream) {
	j.OnEvent = func(did, name, rev string, deleted bool) {
		for _, f := range n.followList(context.Background()) {
			if f.DID == did && f.Name == name {
				n.FollowRefresh(context.Background(), apiv0.FollowRefreshRequest{Dataset: f.Dataset})
			}
		}
	}
	n.names.mu.Lock()
	n.names.jetstream = j
	n.names.mu.Unlock()
}

// FollowLoop polls follows every FollowPoll (jittered) after an initial refresh, until
// ctx ends, keeping the Jetstream filter (when configured) at the followed DIDs. The
// daemon runs it beside Maintain.
func (n *Local) FollowLoop(ctx context.Context) {
	kick := make(chan struct{}, 1)
	n.names.mu.Lock()
	js := n.names.jetstream
	n.names.kick = kick
	n.names.mu.Unlock()
	if js != nil {
		go js.Run(ctx)
	}
	n.refreshFollows(ctx)
	for {
		if js != nil {
			js.Subscribe(n.followedDIDs(ctx))
		}
		d := FollowPoll + time.Duration(float64(FollowPoll)*0.1*float64(time.Now().UnixNano()%7)/7)
		select {
		case <-ctx.Done():
			return
		case <-kick:
			continue
		case <-time.After(d):
		}
		n.refreshFollows(ctx)
	}
}
