package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/atproto"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/gateway"
	"github.com/whyrusleeping/nu/node/content"
	"github.com/whyrusleeping/nu/node/op"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/node/target"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// Local is the in-process Node: the daemon serves it over the API, and daemonless
// commands run it directly under the repo lock. It owns the store.
type Local struct {
	Repo  *repo.Repo
	Ops   *op.Manager
	Store *store.Store
	// Content backs the gateway.
	Content gateway.Content

	// Key is the node's libp2p identity, loaded from (or created at) $NU_PATH/node.key.
	Key *wire.NodeKey
	// Fetcher brings content the node does not hold; NoFetcher until networking plugs one in.
	Fetcher Fetcher
	// Net is the running host's view of reachability; nil without a host.
	Net Net
	// Atproto resolves identities and fetches verified records and repositories over
	// HTTPS (no swarm needed); nil disables at:// targets. NewLocal sets the default
	// client; tests substitute one with fixture hooks.
	Atproto *atproto.Client

	content  *content.Content
	clusters clusterState
	reps     replicas
	public   publicState
	started  time.Time
	// API and Gateway are the bound listen addresses, set by the daemon.
	API, Gateway string
	// work tracks operation goroutines so Close can wait for them.
	work sync.WaitGroup
}

// GCGrace is the grace period a node passes to store GC (store.GCOptions.Grace), so the
// objects of an add or fetch in progress survive until they are pinned. Automatic GC
// (store.GCIfNeeded, D17 A7) runs from the daemon's Maintain loop and on admission
// shortfalls.
const GCGrace = time.Hour

// NewLocal opens the store in the repo (creating it on first use), loads or creates the
// node key, and returns a node with a fresh operation manager and no fetcher. Close it
// when done.
func NewLocal(r *repo.Repo) (*Local, error) {
	key, err := LoadKey(r)
	if err != nil {
		return nil, err
	}
	limits, err := storeLimits(r)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(r.Root(), store.Options{Limits: limits})
	if errors.Is(err, store.ErrNewerSchema) {
		return nil, apiv0.Errorf(apiv0.CodeNotSupported, "the store in %s was upgraded by a newer %s: %v", r.Root(), nu.Name, err).
			WithAction("run that newer release; downgrades are not supported (restore a backup and export taken before the upgrade instead)")
	}
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "cannot open the store in %s: %v", r.Root(), err).
			WithAction("check that the directory is writable and not in use by an older or newer " + nu.Name)
	}
	c := content.New(st)
	n := &Local{Repo: r, Ops: op.NewManager(), Store: st, Content: c.Gateway(), Key: key, Fetcher: NoFetcher{},
		Atproto: atproto.New(atproto.Config{}), content: c, started: time.Now()}
	c.SetDatasets(n.ResolveDataset)
	return n, nil
}

// LoadKey loads the node key from $NU_PATH/node.key, creating it (0600) on first use.
// Every node of a repo shares this identity; `nu init` creates it so a fresh repo is
// complete, and NewLocal creates it for repos initialized by older builds.
func LoadKey(r *repo.Repo) (*wire.NodeKey, error) {
	key, err := wire.LoadOrCreateNodeKey(r.Path(repo.KeyFile))
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "cannot load the node key %s: %v", r.Path(repo.KeyFile), err).
			WithAction("the file must be a 0600 libp2p Ed25519 key; move it away to generate a new identity")
	}
	return key, nil
}

var _ Node = (*Local)(nil)

// Close stops the node: running operations are cancelled and waited for, then the store
// is closed. The owner closes the network (Fetcher/Net) between those two steps if it
// runs one; Close does not.
func (n *Local) Close() error {
	n.Ops.CancelAll()
	n.work.Wait()
	n.DetachNetwork()
	return n.Store.Close()
}

// Wait blocks until running operations have finished (after CancelAll: until they have
// observed the cancellation).
func (n *Local) Wait() { n.work.Wait() }

// Paths returns the repo layout.
func Paths(r *repo.Repo) apiv0.Paths {
	return apiv0.Paths{
		Root:    r.Root(),
		Config:  r.Path(repo.ConfigFile),
		Key:     r.Path(repo.KeyFile),
		DB:      r.Path(repo.DBFile),
		Blobs:   r.Path(repo.BlobDir),
		APIFile: r.Path(repo.APIFile),
	}
}

func (n *Local) Status(ctx context.Context) (*apiv0.Status, error) {
	st := &apiv0.Status{
		Version: nu.Version,
		Daemon: apiv0.Daemon{
			PID:     os.Getpid(),
			Started: n.started,
			API:     n.API,
			Gateway: n.Gateway,
			Public:  n.PublicOrigin(),
		},
		Paths:      Paths(n.Repo),
		Operations: n.Ops.Counts(),
	}
	ss, err := n.Store.Stats()
	if err != nil {
		return nil, err
	}
	st.Store = storeStatus(ss)
	st.Network = n.networkStatus()
	return st, nil
}

func (n *Local) Operations(ctx context.Context, cursor string, limit int) (*apiv0.Page[apiv0.Operation], error) {
	return apiv0.PageSlice(n.Ops.List(), cursor, limit, n.Ops.Epoch())
}

func (n *Local) op(id string) (*op.Operation, error) {
	o, ok := n.Ops.Get(id)
	if !ok {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "no operation %q", id).
			WithAction("list operations with `" + nu.Name + " operations`; finished operations are kept for a limited time")
	}
	return o, nil
}

func (n *Local) Operation(ctx context.Context, id string) (*apiv0.Operation, error) {
	o, err := n.op(id)
	if err != nil {
		return nil, err
	}
	s := o.Snapshot()
	return &s, nil
}

func (n *Local) OperationEvents(ctx context.Context, id string, since int64) (EventStream, error) {
	o, err := n.op(id)
	if err != nil {
		return nil, err
	}
	return o.Events(since), nil
}

func (n *Local) WaitOperation(ctx context.Context, id string, timeout time.Duration) (*apiv0.Operation, error) {
	o, err := n.op(id)
	if err != nil {
		return nil, err
	}
	s, err := o.Wait(ctx, timeout)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (n *Local) CancelOperation(ctx context.Context, id string) error {
	o, err := n.op(id)
	if err != nil {
		return err
	}
	if o.Snapshot().Done() {
		return apiv0.Errorf(apiv0.CodeConflict, "operation %q has already finished", id)
	}
	o.Cancel()
	return nil
}

// start runs work as an operation of the given kind in its own goroutine (the request
// context ends with the request; work watches the operation's context instead) and
// returns the initial snapshot.
func (n *Local) start(kind, target string, work func(o *op.Operation) (any, error)) *apiv0.Operation {
	o := n.Ops.Start(kind, target)
	n.work.Add(1)
	go func() {
		defer n.work.Done()
		res, err := work(o)
		o.Finish(res, apiErr(err))
	}()
	snap := o.Snapshot()
	return &snap
}

// resolve parses a CID or link target (with optional path) and resolves it in the store.
func (n *Local) resolve(t string) (*content.Node, cid.CID, string, error) {
	pt, root, err := n.parseTarget(context.Background(), t)
	if err != nil {
		return nil, cid.CID{}, "", err
	}
	nd, err := n.content.Resolve(root, pt.Path)
	return nd, root, pt.Path, err
}

// resolveOrFetch is resolve for readers (cat, ls): content that is not held is fetched
// first, with the link's hints, and must be complete under the resolved node.
func (n *Local) resolveOrFetch(ctx context.Context, t string) (*content.Node, cid.CID, string, error) {
	pt, root, err := n.parseTarget(ctx, t)
	if err != nil {
		return nil, cid.CID{}, "", err
	}
	nd, err := n.ensure(ctx, root, pt.Path, targetHints(pt), nil, nil)
	return nd, root, pt.Path, err
}

func (n *Local) parseTarget(ctx context.Context, t string) (target.Target, cid.CID, error) {
	pt, err := target.Parse(t)
	if err != nil {
		return pt, cid.CID{}, err
	}
	if err := pt.Supported(); err != nil {
		return pt, cid.CID{}, err
	}
	if pt.Kind == target.KindDataset {
		root, err := n.resolveDataset(ctx, pt)
		return pt, root, err
	}
	if pt.Kind == target.KindAtproto {
		return pt, cid.CID{}, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is an atproto target, not content by CID", pt.Raw).
			WithAction("records: `" + nu.Name + " get at://<did>/<collection>/<rkey>` (API: GET /at/record); repositories: `" + nu.Name + " get at://<did>` (POST /at/snapshot); browse held snapshots with ls and cat")
	}
	root, err := cid.Parse(pt.CID)
	if err != nil {
		return pt, cid.CID{}, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a CID", pt.CID)
	}
	return pt, root, nil
}

func (n *Local) Cat(ctx context.Context, t string) (io.ReadCloser, error) {
	if isAt(t) {
		return n.atCat(ctx, t)
	}
	nd, _, _, err := n.resolveOrFetch(ctx, t)
	if err != nil {
		return nil, err
	}
	if nd.Kind == gateway.KindDir {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is a directory", t).WithAction("list it with `ls`, or cat a file inside it")
	}
	return n.content.Open(nd)
}

func (n *Local) Ls(ctx context.Context, t string) (*apiv0.Listing, error) {
	if isAt(t) {
		return n.atLs(ctx, t)
	}
	nd, root, p, err := n.resolveOrFetch(ctx, t)
	if err != nil {
		return nil, err
	}
	l := &apiv0.Listing{Target: root.String(), Entries: []apiv0.Entry{}}
	if p != "" {
		l.Target += "/" + p
	}
	if nd.Kind != gateway.KindDir {
		l.Entries = append(l.Entries, apiv0.Entry{Name: nd.Name, CID: nd.CID.String(), Type: string(nd.Kind), Size: nd.Size})
		return l, nil
	}
	for e, err := range n.content.Entries(nd) {
		if err != nil {
			return nil, err
		}
		l.Entries = append(l.Entries, apiv0.Entry{Name: e.Name, CID: e.CID.String(), Type: string(e.Kind), Size: e.Size})
	}
	return l, nil
}

// pinFor is the explicit pin of c: recursive under the nu-tree profile for structure,
// direct for a raw blob.
func pinFor(c cid.CID, name string) store.Pin {
	p := store.Pin{CID: c, Mode: store.Recursive, Profile: "nu-tree", Source: store.SourceExplicit, Name: name}
	if c.Codec() == cid.Raw {
		p.Mode = store.Direct
	}
	return p
}

// missing is the not_found error for content held in part (or not at all).
func missing(c cid.CID, missing, objects int) *apiv0.Error {
	if objects == 1 {
		return content.NotHeld(c)
	}
	return content.Incomplete(c, missing, objects)
}

func apiPin(p store.Pin) apiv0.Pin {
	return apiv0.Pin{CID: p.CID.String(), Name: p.Name, Mode: p.Mode, Source: p.Source}
}

func parseCID(s string) (cid.CID, error) {
	c, err := cid.Parse(s)
	if err != nil {
		return cid.CID{}, apiv0.Errorf(apiv0.CodeInvalidArgument, "%q is not a CID", s).WithAction("give a b… DASL CID")
	}
	return c, nil
}

func (n *Local) PinAdd(ctx context.Context, req apiv0.PinRequest) (*apiv0.Operation, error) {
	c, err := parseCID(req.CID)
	if err != nil {
		return nil, err
	}
	p := pinFor(c, req.Name)
	return n.start("pin", c.String(), func(o *op.Operation) (any, error) {
		o.Stage("checking")
		var objects int64
		last := time.Now()
		st, err := n.Store.Walk(o.Context(), p, func(cid.CID, bool) error {
			objects++
			if time.Since(last) > 100*time.Millisecond {
				o.Progress(objects, 0, "objects")
				last = time.Now()
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		o.Progress(int64(st.Objects), 0, "objects")
		if !st.Complete() {
			return nil, missing(c, st.Missing, st.Objects)
		}
		o.Stage("pinning")
		if err := n.Store.Pin(p); err != nil {
			return nil, err
		}
		return apiPin(p), nil
	}), nil
}

func (n *Local) PinRm(ctx context.Context, s string) error {
	c, err := parseCID(s)
	if err != nil {
		return err
	}
	pins, err := n.Store.Pins()
	if err != nil {
		return err
	}
	removed := 0
	for _, p := range pins {
		if p.CID != c || p.Source != store.SourceExplicit {
			continue
		}
		if ok, err := n.Store.Unpin(p); err != nil {
			return err
		} else if ok {
			removed++
		}
	}
	if removed == 0 {
		return apiv0.Errorf(apiv0.CodeNotFound, "%s is not pinned", c).WithAction("list pins with `" + nu.Name + " pin ls`")
	}
	return nil
}

func (n *Local) Pins(ctx context.Context, cursor string, limit int) (*apiv0.Page[apiv0.Pin], error) {
	pins, err := n.Store.Pins()
	if err != nil {
		return nil, err
	}
	items := make([]apiv0.Pin, 0, len(pins))
	for _, p := range pins {
		items = append(items, apiPin(p))
	}
	return apiv0.PageSlice(items, cursor, limit, n.Ops.Epoch())
}

func (n *Local) DagGet(ctx context.Context, t string) (json.RawMessage, error) {
	pt, err := target.Parse(t)
	if err != nil {
		return nil, err
	}
	if err := pt.Supported(); err != nil {
		return nil, err
	}
	if pt.Kind == target.KindAtproto {
		_, _, err := n.parseTarget(ctx, t) // the at:// explanation
		return nil, err
	}
	c, err := parseCID(pt.CID)
	if err != nil {
		return nil, err
	}
	v, err := n.dagLoad(c)
	if err != nil {
		return nil, err
	}
	// A path selects into the object: map keys and array indexes, following links.
	if pt.Path != "" {
		for _, seg := range strings.Split(pt.Path, "/") {
			if l, ok := v.(cid.CID); ok {
				if v, err = n.dagLoad(l); err != nil {
					return nil, err
				}
			}
			switch x := v.(type) {
			case map[string]any:
				var ok bool
				if v, ok = x[seg]; !ok {
					return nil, apiv0.Errorf(apiv0.CodeNotFound, "no key %q under %s", seg, pt.CID)
				}
			case []any:
				i, err := strconv.Atoi(seg)
				if err != nil || i < 0 || i >= len(x) {
					return nil, apiv0.Errorf(apiv0.CodeNotFound, "no index %q under %s", seg, pt.CID)
				}
				v = x[i]
			default:
				return nil, apiv0.Errorf(apiv0.CodeNotFound, "%q: cannot descend into a scalar under %s", seg, pt.CID)
			}
		}
	}
	return drisl.ToJSON(v)
}

func (n *Local) dagLoad(c cid.CID) (any, error) {
	if c.Codec() != cid.DRISL {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%s is a raw blob, not a DRISL object", c).WithAction("read its bytes with `cat`")
	}
	data, err := n.Store.Get(c)
	if errors.Is(err, store.ErrNotFound) {
		return nil, content.NotHeld(c)
	}
	if err != nil {
		return nil, err
	}
	return drisl.Decode(data)
}

func (n *Local) DagPut(ctx context.Context, doc io.Reader) (*apiv0.DagPutResult, error) {
	data, err := io.ReadAll(io.LimitReader(doc, cid.MaxWholeObject+1))
	if err != nil {
		return nil, err
	}
	if len(data) > cid.MaxWholeObject {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "document exceeds the %d-byte object limit", cid.MaxWholeObject)
	}
	v, err := drisl.FromJSON(data, drisl.JSONOptions{})
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err).
			WithAction("give atproto-style JSON: links as {\"$link\": \"<cid>\"}, bytes as {\"$bytes\": \"<base64>\"}")
	}
	c, enc, err := drisl.Sum(v)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err)
	}
	if err := n.Store.Put(c, enc); err != nil {
		return nil, apiErr(err)
	}
	return &apiv0.DagPutResult{CID: c.String()}, nil
}
