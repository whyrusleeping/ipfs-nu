package fetch

import (
	"context"
	"errors"
	"log/slog"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/proto"
)

// treeSync fetches root's structure and small blobs over /nu/tree/1 (layer 2) from the
// first peers that support it, resuming across requests and peers from the verified
// cursor. It is only an accelerator: whatever it does not deliver (large blobs, items a
// peer lacks, everything after a failure) the layer-1 walk fetches afterwards.
func (r *run) treeSync(ctx context.Context, root cid.CID) {
	req := proto.TreeRequest{Root: root, Scope: proto.ScopeSubtree, InlineMax: proto.MaxInline, Have: r.opts.Have}
	if r.opts.Want != nil {
		req.InlineMax = 0 // blobs are selected one by one
	}
	if r.opts.Profile == proto.ProfileAtrepo || r.opts.Profile == "atrepo+blobs" {
		req.Profile, req.InlineMax = proto.ProfileAtrepo, 0
	}
	for _, p := range r.ranked(root, r.sw.live()) {
		for fails := 0; fails < maxFails && r.sw.usable(p); {
			rctx, cancel := r.timeout(ctx)
			res, err := proto.FetchTree(rctx, p.Open, r.st, req)
			cancel()
			req.Cursor = res.Cursor
			switch {
			case err == nil && !res.More:
				return
			case err == nil:
				continue
			case ctx.Err() != nil:
				return
			case errors.Is(err, wire.ErrNotSupported), errors.As(err, new(*proto.StatusError)):
				fails = maxFails // refusals and old peers: try the next peer
				continue
			}
			slog.Debug("fetch: tree request failed", "peer", p.ID(), "root", root, "err", err)
			r.sw.report(ctx, p, err)
			fails++
		}
	}
}
