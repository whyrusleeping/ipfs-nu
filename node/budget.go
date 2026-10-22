package node

import (
	"context"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/content"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/store"
)

// Store budgets (D17 A5–A8): the `[store]` config section becomes store.Limits, budget
// refusals become insufficient_storage errors, and the daemon runs GCIfNeeded.

// MaintainInterval is how often a daemon checks the GC watermark and free-space reserve.
const MaintainInterval = 10 * time.Minute

// storeLimits reads the budgets from config.toml.
func storeLimits(r *repo.Repo) (store.Limits, error) {
	cfg, err := r.Config()
	if err != nil {
		return store.Limits{}, apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err).
			WithAction("fix the value in config.toml; sizes are bytes with an optional K/M/G/T/P suffix, e.g. `" + nu.Name + " config set store.limit 50G` (0 for no limit)")
	}
	return store.Limits{Physical: int64(cfg.Store.Limit), FreeReserve: int64(cfg.Store.FreeReserve),
		Watermark: cfg.Store.GCWatermark, Grace: GCGrace, StateReserve: int64(cfg.Store.StateReserve)}, nil
}

// apiErr maps store errors into the API error contract (content.StoreError).
func apiErr(err error) error { return content.StoreError(err) }

// storeStatus is the status section from the store's accounting (A8).
func storeStatus(st store.Stats) *apiv0.StoreStatus {
	return &apiv0.StoreStatus{Objects: st.Objects, Bytes: st.Bytes, Pins: st.Pins,
		Blobs: st.Blobs, Outboards: st.Outboards, Partials: st.Partials, DB: st.DB, Staging: st.Staging,
		Reserved: st.Reserved, NoCopy: st.NoCopy, Free: st.Free, Limit: st.Limit, Reserve: st.Reserve,
		Headroom: st.Headroom, Pressure: st.Pressure}
}

// Maintain runs the store's automatic GC (A7) every MaintainInterval until ctx ends; the
// daemon runs it in its own goroutine. log may be nil.
func (n *Local) Maintain(ctx context.Context, log func(msg string, args ...any)) {
	t := time.NewTicker(MaintainInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ran, res, err := n.Store.GCIfNeeded(ctx)
		if log != nil && (ran || err != nil) {
			log("store gc", "ran", ran, "blocks", res.Blocks, "blobs", res.Blobs, "partials", res.Partials, "orphans", res.Orphans, "err", err)
		}
	}
}
