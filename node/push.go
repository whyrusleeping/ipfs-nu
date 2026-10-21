package node

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/httpsrc"
)

// PushNode is the item push part of a node (docs/collections.md "Ingest", docs/api.md
// "Item pushes"); Local and the HTTP client implement it.
type PushNode interface {
	// CollectionPush accepts an item push; with wait > 0 it then waits (up to wait) until
	// the push is written or failed.
	CollectionPush(ctx context.Context, name string, req apiv0.CollectionPushRequest, wait time.Duration) (*apiv0.CollectionPush, error)
	// CollectionPushBatch accepts several pushes, answering one line per request.
	CollectionPushBatch(ctx context.Context, name string, reqs []apiv0.CollectionPushRequest) ([]apiv0.CollectionPushBatchLine, error)
	// CollectionRemove removes an item from this node's writer head.
	CollectionRemove(ctx context.Context, name, path string, wait time.Duration) (*apiv0.CollectionPush, error)
	// CollectionPushes follows the push log after a cursor ("0" from the start).
	CollectionPushes(ctx context.Context, name, since string, limit int) (*apiv0.Page[apiv0.CollectionPush], error)
	// CollectionUpload stores a blob for a later push (size -1 when unknown).
	CollectionUpload(ctx context.Context, name string, r io.Reader, size int64) (*apiv0.CollectionUpload, error)
}

var _ PushNode = (*Local)(nil)

// IngestHTTP adjusts the HTTPS client policy of URL imports (tests: resolver and dialer
// hooks); the config keys ingest.allow_http and ingest.allow_ips are applied first.
var IngestHTTP func(*httpsrc.Config)

// httpClient builds the URL-import client from trusted local config: ingest.allow_http
// (plain HTTP sources, e.g. a producer's LAN web seed) and ingest.allow_ips (address
// prefixes exempt from the SSRF guard, e.g. that producer's subnet).
func (n *Local) httpClient() *httpsrc.Client {
	var cfg httpsrc.Config
	if v, err := n.Repo.ConfigGet("ingest.allow_http"); err == nil {
		cfg.AllowHTTP, _ = v.(bool)
	}
	if v, err := n.Repo.ConfigGet("ingest.allow_ips"); err == nil {
		list, _ := v.([]any)
		for _, x := range list {
			s, _ := x.(string)
			if p, err := netip.ParsePrefix(s); err == nil {
				cfg.AllowedIPs = append(cfg.AllowedIPs, p)
			} else if a, err := netip.ParseAddr(s); err == nil {
				cfg.AllowedIPs = append(cfg.AllowedIPs, netip.PrefixFrom(a, a.BitLen()))
			}
		}
	}
	if IngestHTTP != nil {
		IngestHTTP(&cfg)
	}
	return httpsrc.New(cfg)
}

func (n *Local) CollectionPush(ctx context.Context, name string, req apiv0.CollectionPushRequest, wait time.Duration) (*apiv0.CollectionPush, error) {
	m, err := n.collections()
	if err != nil {
		return nil, err
	}
	p, err := m.Push(name, req)
	if err != nil || wait <= 0 {
		return p, err
	}
	w, err := m.WaitPush(ctx, name, p.Path, p.ID, wait)
	if w != nil && w.ID == p.ID {
		w.Duplicate = p.Duplicate
	}
	return w, err
}

func (n *Local) CollectionPushBatch(ctx context.Context, name string, reqs []apiv0.CollectionPushRequest) ([]apiv0.CollectionPushBatchLine, error) {
	m, err := n.collections()
	if err != nil {
		return nil, err
	}
	out := make([]apiv0.CollectionPushBatchLine, 0, len(reqs))
	for i, req := range reqs {
		l := apiv0.CollectionPushBatchLine{Line: i + 1, Path: req.Path}
		if l.Push, err = m.Push(name, req); err != nil {
			l.Error = asError(err)
		}
		out = append(out, l)
	}
	return out, nil
}

// asError converts any error to the envelope's shape.
func asError(err error) *apiv0.Error {
	var ae *apiv0.Error
	if errors.As(apiErr(err), &ae) {
		return ae
	}
	return apiv0.Errorf(apiv0.CodeFailed, "%v", err)
}

func (n *Local) CollectionRemove(ctx context.Context, name, path string, wait time.Duration) (*apiv0.CollectionPush, error) {
	m, err := n.collections()
	if err != nil {
		return nil, err
	}
	p, err := m.Remove(name, path)
	if err != nil || wait <= 0 {
		return p, err
	}
	return m.WaitPush(ctx, name, p.Path, p.ID, wait)
}

func (n *Local) CollectionPushes(ctx context.Context, name, since string, limit int) (*apiv0.Page[apiv0.CollectionPush], error) {
	m, err := n.collections()
	if err != nil {
		return nil, err
	}
	return m.Pushes(name, since, limit)
}

func (n *Local) CollectionUpload(ctx context.Context, name string, r io.Reader, size int64) (*apiv0.CollectionUpload, error) {
	m, err := n.collections()
	if err != nil {
		return nil, err
	}
	u, err := m.Upload(name, r, size)
	return u, apiErr(err)
}
