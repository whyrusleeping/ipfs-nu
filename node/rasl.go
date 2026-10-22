package node

import (
	"context"
	"errors"
	"sync"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/fisl"
	"github.com/whyrusleeping/nu/node/link"
	"github.com/whyrusleeping/nu/node/target"
	"github.com/whyrusleeping/nu/rasl"
)

// RASL retrieval (D21, backlog 0052): a `rasl://<cid>/?hint=host` target names HTTPS
// hosts that may serve the CID at /.well-known/rasl/<cid>. The hosts become an HTTPS
// whole-object source (D15) that the fetcher asks alongside Nu peers; without networking
// they are the only source. Every object is verified by CID before it is stored, a host
// that serves wrong bytes is demoted on the shared httpsrc client, and the SSRF policy of
// URL imports applies (loopback, private and link-local hints are refused unless
// ingest.allow_ips says otherwise).

// hints are the sources a target names besides lookup: share-link peers and RASL hosts.
type hints struct {
	Peers []link.Provider
	Hosts []string
}

func targetHints(pt target.Target) hints {
	h := hints{Hosts: pt.Hosts}
	if pt.Link != nil {
		h.Peers = pt.Link.Providers
	}
	return h
}

// SourceFetcher is a Fetcher that can also use an HTTPS whole-object source (RASL hints)
// alongside its peers; node/swarm implements it.
type SourceFetcher interface {
	FetchWithSource(ctx context.Context, root cid.CID, peers []link.Provider, src fetch.Source, progress func(done, total int64)) error
}

// RASLHook adjusts the RASL source policy (tests: plain-HTTP scheme to a local server,
// together with IngestHTTP's address exemption). FISLHook does the same for the FISL
// source, after RASLHook has adjusted its retrieval config.
var (
	RASLHook func(*rasl.Config)
	FISLHook func(*fisl.Config)
)

func (n *Local) raslConfig() (rasl.Config, error) {
	if c, _ := n.netCluster(); c != nil && c.Mode() == cluster.ModeIsolated {
		return rasl.Config{}, apiv0.Errorf(apiv0.CodeForbidden, "this node is in an isolated cluster, which has no egress to RASL hosts").
			WithAction("fetch the content from a cluster member, or add it from a local path")
	}
	cfg := rasl.Config{Client: n.httpClient(), Store: n.Store, Dir: n.Repo.Path("staging")}
	if RASLHook != nil {
		RASLHook(&cfg)
	}
	return cfg, nil
}

func (n *Local) raslSource(hosts []string) (*rasl.Source, error) {
	cfg, err := n.raslConfig()
	if err != nil {
		return nil, err
	}
	return rasl.NewSource(cfg, hosts), nil
}

// FISL discovery (D21 "FISL", backlog 0056): `fisl.indexers` (a list of base URLs, off
// by default) names indexers asked for the locations of every object a user fetch
// needs, alongside the peers and any RASL hints. The located https URLs are fetched
// through the same verified retrieval as RASL hints. A private or isolated cluster
// member never queries indexers. Invalid indexer URLs are skipped.
func (n *Local) fislSource() *fisl.Source {
	v, err := n.Repo.ConfigGet("fisl.indexers")
	if err != nil {
		return nil
	}
	list, _ := v.([]any)
	var indexers []string
	for _, x := range list {
		s, _ := x.(string)
		if u, err := fisl.ParseIndexer(s); err == nil {
			indexers = append(indexers, u)
		}
	}
	if len(indexers) == 0 {
		return nil
	}
	if c, _ := n.netCluster(); c != nil && (c.Mode() == cluster.ModePrivate || c.Mode() == cluster.ModeIsolated) {
		return nil
	}
	rc, err := n.raslConfig()
	if err != nil {
		return nil
	}
	cfg := fisl.Config{Indexers: indexers, Retrieve: rc}
	if v, err := n.Repo.ConfigGet("ingest.allow_http"); err == nil {
		cfg.AllowHTTP, _ = v.(bool)
	}
	if FISLHook != nil {
		FISLHook(&cfg)
	}
	return fisl.NewSource(cfg)
}

// sources races HTTPS sources (RASL hints, FISL locations): the first to make the
// object held wins and cancels the others; when all fail the errors are joined.
type sources []fetch.Source

func (ss sources) FetchObject(ctx context.Context, c cid.CID, size uint64) error {
	if len(ss) == 1 {
		return ss[0].FetchObject(ctx, c, size)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make([]error, len(ss))
	var wg sync.WaitGroup
	for i, s := range ss {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if errs[i] = s.FetchObject(ctx, c, size); errs[i] == nil {
				cancel()
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err == nil {
			return nil
		}
	}
	return errors.Join(errs...)
}

// fetch brings c (and everything under it) in through the Fetcher with the target's
// hints. RASL hosts and configured FISL indexers join the peers when the Fetcher
// supports sources; a node without networking fetches from them alone.
func (n *Local) fetch(ctx context.Context, c cid.CID, h hints, progress func(done, total int64)) error {
	var ss sources
	if len(h.Hosts) > 0 {
		rs, err := n.raslSource(h.Hosts)
		if err != nil {
			return err
		}
		ss = append(ss, rs)
	}
	if fs := n.fislSource(); fs != nil {
		ss = append(ss, fs)
	}
	if len(ss) == 0 {
		return n.Fetcher.Fetch(ctx, c, h.Peers, progress)
	}
	var src fetch.Source = ss
	if sf, ok := n.Fetcher.(SourceFetcher); ok {
		return sf.FetchWithSource(ctx, c, h.Peers, src, progress)
	}
	opts := fetch.Options{Source: src}
	if progress != nil {
		opts.Progress = func(p fetch.Progress) {
			if p.Stage == fetch.StageBlobs || p.Stage == fetch.StageDone {
				progress(p.Bytes, p.BytesTotal)
			}
		}
	}
	_, err := fetch.New(fetch.Config{Store: n.Store}).Fetch(ctx, c, opts)
	if err == nil || ctx.Err() != nil {
		return err
	}
	return ClassifyRASL(c, err)
}

// ClassifyRASL maps a fetch failure whose only sources were HTTPS locations (RASL
// hosts, FISL indexers) onto the API error contract: not_found when every host answered
// 404 or no indexer knows a location, unavailable otherwise, and insufficient_storage
// when the store refused the bytes.
func ClassifyRASL(root cid.CID, err error) error {
	var ae *apiv0.Error
	if errors.As(err, &ae) {
		return ae
	}
	if serr := apiErr(err); serr != err {
		return serr
	}
	var se *rasl.SourceError
	if errors.As(err, &se) && se.NotFound() {
		return apiv0.Errorf(apiv0.CodeNotFound, "no RASL host has %s", se.CID).
			WithAction("check the hints in the link; the host may have removed the content").WithDetail("cause", err.Error())
	}
	if errors.Is(err, fisl.ErrNoLocation) && se == nil {
		return apiv0.Errorf(apiv0.CodeNotFound, "no configured FISL indexer knows a location for %s", root).
			WithAction("the content may not be announced yet; use a link with hints or a Nu provider").WithDetail("cause", err.Error())
	}
	if errors.Is(err, rasl.ErrMismatch) {
		return apiv0.Errorf(apiv0.CodeFailed, "an HTTPS location served bytes that do not match %s", root).
			WithAction("the location is demoted for a while; try other hints or a Nu provider").WithDetail("cause", err.Error())
	}
	return apiv0.Errorf(apiv0.CodeUnavailable, "no HTTPS location could serve %s", root).
		WithAction("check that the hinted hosts and configured FISL indexers are reachable over HTTPS, or use a nu:// link with a provider").WithDetail("cause", err.Error())
}
