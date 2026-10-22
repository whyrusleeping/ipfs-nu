package node

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/collection"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/store"
)

// CollectionNode is the collections part of a node (docs/collections.md, docs/api.md
// "Collections"); Local and the HTTP client implement it.
type CollectionNode interface {
	CollectionStatus(ctx context.Context, name string) (*apiv0.CollectionStatus, error)
	CollectionItems(ctx context.Context, name string, q apiv0.CollectionItemsQuery) (*apiv0.Page[apiv0.CollectionItem], error)
	CollectionItem(ctx context.Context, name, path string) (*apiv0.CollectionItem, error)
	CollectionRules(ctx context.Context, name string) (*apiv0.CollectionRules, error)
	CollectionSetRules(ctx context.Context, name string, rules json.RawMessage, dryRun bool) (*apiv0.CollectionRules, error)
	CollectionHold(ctx context.Context, name, path string, hold bool) (*apiv0.CollectionItem, error)
	CollectionConflicts(ctx context.Context, name string) ([]apiv0.CollectionConflict, error)
}

var _ CollectionNode = (*Local)(nil)

// CatalogFetcher is implemented by fetchers that can fetch a collection catalog alone
// (the swarm).
type CatalogFetcher interface {
	FetchCatalog(ctx context.Context, root cid.CID, skip func(cid.CID) bool) error
}

// HeadInterval overrides head.min_interval (0: the 10 s default); tests shorten it.
var HeadInterval time.Duration

// IngestBackoff is the first retry delay of item URL fetches (0: the default); tests
// shorten it.
var IngestBackoff time.Duration

// CollectionIndex makes nodes this process starts run the holdings index (tests; the
// daemon reads cluster.index from the config).
var CollectionIndex = false

// collectionsLocked opens the collection manager of the networked cluster c (cs.mu held).
func (n *Local) collectionsLocked(c *cluster.Cluster) (*collection.Manager, error) {
	cs := &n.clusters
	if cs.coll != nil {
		return cs.coll, nil
	}
	cfg := collection.Config{DBPath: filepath.Join(n.clusterDir("net"), "collections.db"), Store: n.Store, Cluster: c, Self: n.PeerID(),
		Service: n.ClusterServiceNoLock, Index: CollectionIndex || n.configIndex(), Interval: ClusterInterval, HTTP: n.httpClient(),
		IngestBackoff: IngestBackoff, HeadInterval: HeadInterval,
		GC: func(ctx context.Context) { _, _ = n.Store.GC(ctx, store.GCOptions{Grace: GCGrace}) },
		Changed: func() {
			if svc := n.clusters.svcPtr.Load(); svc != nil {
				svc.KickHeartbeat()
			}
		}}
	if f := n.Fetcher; f != nil {
		if _, none := f.(NoFetcher); !none {
			cfg.FetchItem = func(ctx context.Context, root cid.CID) error { return f.Fetch(ctx, root, nil, nil) }
			if cf, ok := f.(CatalogFetcher); ok {
				cfg.FetchCatalog = cf.FetchCatalog
			}
		}
	}
	m, err := collection.Open(cfg)
	if err != nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "cannot open the collection state: %v", err)
	}
	cs.coll = m
	cs.collPtr.Store(m)
	return m, nil
}

// configIndex reads cluster.index from the repo config.
func (n *Local) configIndex() bool {
	v, err := n.Repo.ConfigGet("cluster.index")
	if err != nil {
		return false
	}
	b, _ := v.(bool)
	return b
}

// ClusterServiceNoLock returns the running cluster service without waiting for cluster
// operations in progress (the collection manager calls it while they hold the lock).
func (n *Local) ClusterServiceNoLock() *cluster.Service {
	return n.clusters.svcPtr.Load()
}

// Collections returns the collection manager of the networked cluster (nil if none).
func (n *Local) Collections() *collection.Manager {
	cs := &n.clusters
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c, err := n.netClusterLocked()
	if err != nil || c == nil {
		return nil
	}
	m, _ := n.collectionsLocked(c)
	return m
}

func (n *Local) collections() (*collection.Manager, error) {
	m := n.Collections()
	if m == nil {
		return nil, noCluster()
	}
	return m, nil
}

// collectionPolicy builds a dataset's collection field from a create request.
func collectionPolicy(req apiv0.DatasetCreateRequest) (*cluster.CollectionPolicy, error) {
	if !req.Collection {
		if len(req.Facets) > 0 || req.Merge != "" || req.Expire != "" {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "facets, merge and expire apply to collections").WithAction("add --collection")
		}
		return nil, nil
	}
	p := &cluster.CollectionPolicy{Merge: req.Merge, Facets: map[string]cluster.Facet{}}
	specs := map[string]collection.FacetSpec{}
	for k, f := range req.Facets {
		p.Facets[k] = cluster.Facet{Ptr: f.Ptr, Type: f.Type}
		specs[k] = collection.FacetSpec{Ptr: f.Ptr, Type: f.Type}
	}
	if err := collection.ValidateFacets(specs); err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err)
	}
	if p.Merge != "" && p.Merge != cluster.MergeUnion && p.Merge != cluster.MergeLWW {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "merge must be union or lww")
	}
	if req.Expire != "" {
		facet, after, ok := strings.Cut(req.Expire, "=")
		if _, err := collection.ParseDuration(after); !ok || err != nil || specs[facet].Type != collection.TypeTime {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "expire must be <time facet>=<duration>, e.g. sensed=400d")
		}
		p.Expire = &cluster.Expire{Facet: facet, After: after}
	}
	return p, nil
}

func (n *Local) CollectionStatus(ctx context.Context, name string) (*apiv0.CollectionStatus, error) {
	m, err := n.collections()
	if err != nil {
		return nil, err
	}
	return m.Status(name)
}

func (n *Local) CollectionItems(ctx context.Context, name string, q apiv0.CollectionItemsQuery) (*apiv0.Page[apiv0.CollectionItem], error) {
	m, err := n.collections()
	if err != nil {
		return nil, err
	}
	return m.Items(name, q)
}

func (n *Local) CollectionItem(ctx context.Context, name, path string) (*apiv0.CollectionItem, error) {
	m, err := n.collections()
	if err != nil {
		return nil, err
	}
	return m.Item(name, path)
}

func (n *Local) CollectionRules(ctx context.Context, name string) (*apiv0.CollectionRules, error) {
	m, err := n.collections()
	if err != nil {
		return nil, err
	}
	rules, err := m.Rules(name)
	if err != nil {
		return nil, err
	}
	return m.SetRules(name, rules, true)
}

func (n *Local) CollectionSetRules(ctx context.Context, name string, raw json.RawMessage, dryRun bool) (*apiv0.CollectionRules, error) {
	m, err := n.collections()
	if err != nil {
		return nil, err
	}
	var rules []collection.Rule
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &rules); err != nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "bad rules: %v", err).
				WithAction("rules are a list of flat rule objects, see docs/collections.md \"Selection\"")
		}
	}
	return m.SetRules(name, rules, dryRun)
}

func (n *Local) CollectionHold(ctx context.Context, name, path string, hold bool) (*apiv0.CollectionItem, error) {
	m, err := n.collections()
	if err != nil {
		return nil, err
	}
	if hold {
		err = m.Hold(name, path)
	} else {
		err = m.Release(name, path)
	}
	if err != nil {
		return nil, err
	}
	return m.Item(name, path)
}

func (n *Local) CollectionConflicts(ctx context.Context, name string) ([]apiv0.CollectionConflict, error) {
	m, err := n.collections()
	if err != nil {
		return nil, err
	}
	return m.Conflicts(name)
}
