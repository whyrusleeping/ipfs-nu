package collection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/store"
)

// Followed collections (0058; docs/collections.md "Following collections",
// docs/follow.md "Follow scopes and quotas"): a `nu follow <name> --select rules` dataset
// is a collection whose single writer is the followed name. The admin that resolves the
// name writes each verified target as the head; the manager syncs that head's catalog
// whole (the nu-catalog pin), reads the facets the publisher declared in the tree
// header, and holds the items the node's rules select, like a cluster member's local
// rules. Rescue rules need a holdings index and are refused; `max_size` is a budget over
// every rule's holdings. Holdings are reported to no cluster.

// IsFollow reports whether d is a followed collection.
func IsFollow(d *cluster.Dataset) bool {
	return d != nil && d.Collection != nil && d.Source.Name != "" && d.Source.Follow != nil && d.Source.Follow.Select
}

// followWriter is the head to sync for a followed collection: the newest head (the
// follower's own admin wrote it from the proof).
func followWriter(v *cluster.View, name string) []string {
	if h, ok := v.Head(name); ok {
		return []string{h.Signer}
	}
	return nil
}

// headerFacets reads the facet declarations a collection tree header carries
// (`collection.facets`, the merged header of D22). Missing or invalid declarations yield
// no facets.
func (m *Manager) headerFacets(h cid.CID) map[string]FacetSpec {
	out := map[string]FacetSpec{}
	b, err := m.cfg.Store.Get(h)
	if err != nil {
		return out
	}
	hd, err := tree.DecodeHeader(b)
	if err != nil {
		return out
	}
	raw, _ := hd.Collection["facets"].(map[string]any)
	for k, v := range raw {
		f, _ := v.(map[string]any)
		ptr, _ := f["ptr"].(string)
		typ, _ := f["type"].(string)
		out[k] = FacetSpec{Ptr: ptr, Type: typ}
	}
	if err := ValidateFacets(out); err != nil {
		return map[string]FacetSpec{}
	}
	return out
}

// specs returns the facets selection is evaluated against: the dataset's declarations,
// or for a followed collection the ones read from its synced catalog header.
func (m *Manager) specs(name string, d *cluster.Dataset) map[string]FacetSpec {
	if !IsFollow(d) {
		return facetSpecs(d)
	}
	out := map[string]FacetSpec{}
	var fj string
	if err := m.db.QueryRow(`SELECT facets FROM coll WHERE ds = ?`, name).Scan(&fj); err == nil {
		_ = json.Unmarshal([]byte(fj), &out)
	}
	return out
}

// ErrRescueFollow is a rescue rule on a followed collection.
var ErrRescueFollow = errors.New("collection: rescue rules need a cluster holdings index; a follow cannot rescue")

// checkFollowRules refuses what a follower cannot run.
func checkFollowRules(rules []Rule) error {
	for i := range rules {
		if rules[i].Rescue != nil {
			return fmt.Errorf("rule %d: %w", i+1, ErrRescueFollow)
		}
	}
	return nil
}

// followLimit is the follow's max_size: a budget over all holdings (0: none).
func followLimit(d *cluster.Dataset) int64 {
	if IsFollow(d) && d.Source.Follow.MaxSize > 0 {
		return d.Source.Follow.MaxSize
	}
	return 0
}

// SyncedHead returns the head a followed collection's catalog was last synced from (its
// held view) and whether its catalog is held whole.
func (m *Manager) SyncedHead(name string) (cid.CID, bool) {
	row, err := m.loadColl(name)
	if err != nil || len(row.writers) == 0 || !row.writers[0].Head.Defined() {
		return cid.CID{}, false
	}
	return row.writers[0].Head, true
}

// Forget drops a collection's local state (items, holdings, rules, logs) and releases
// its catalog and item pins; it returns the CIDs whose pins were released. Unfollow uses
// it.
func (m *Manager) Forget(name string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, err := m.loadColl(name)
	if err != nil {
		return nil, err
	}
	var released []string
	unpin := func(p store.Pin) {
		if ok, err := m.cfg.Store.Unpin(p); err == nil && ok {
			released = append(released, p.CID.String())
		}
	}
	rows, err := m.db.Query(`SELECT path, entry, src FROM holdings WHERE ds = ? AND state != ?`, name, StatePaused)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p, src string
		var eb []byte
		if rows.Scan(&p, &eb, &src) == nil {
			if c, err := cid.Decode(eb); err == nil {
				unpin(itemPin(name, p, src, c))
			}
		}
	}
	rows.Close()
	for _, w := range row.writers {
		if w.Head.Defined() {
			unpin(catalogPin(w.Head, "catalog/"+name+"/"+w.DID))
		}
	}
	if row.header.Defined() {
		unpin(catalogPin(row.header, "catalog/"+name))
	}
	for _, t := range []string{"items", "ftime", "conflicts", "rules", "manual", "holdings", "hlog", "idx", "idx_cursor", "pending", "coll"} {
		if _, err := m.db.Exec(`DELETE FROM `+t+` WHERE ds = ?`, name); err != nil {
			return released, err
		}
	}
	slices.Sort(released)
	return slices.Compact(released), nil
}

// FollowSummary summarises a followed collection for `follow ls`: the catalog's state
// and the selection counts.
func (m *Manager) FollowSummary(name string) (*apiv0.FollowSelection, error) {
	st, err := m.Status(name)
	if err != nil {
		return nil, err
	}
	out := &apiv0.FollowSelection{Items: st.Items, Rules: st.Rules, Facets: map[string]apiv0.FacetDecl{}, Shortfall: st.Shortfall, Warnings: st.Warnings}
	for k, f := range m.specs(name, mustDataset(m, name)) {
		out.Facets[k] = apiv0.FacetDecl{Ptr: f.Ptr, Type: f.Type}
	}
	for s, n := range st.Local {
		switch s {
		case StateComplete:
			out.Held += n
		case StateFetching:
			out.Fetching += n
		case StatePaused:
			out.Paused += n
		}
	}
	out.Selected = out.Held + out.Fetching + out.Paused
	out.HeldBytes = st.LocalBytes
	if len(st.Writers) > 0 {
		out.Catalog = st.Writers[0].Head
		out.CatalogSynced = st.Writers[0].Synced
	}
	return out, nil
}

func mustDataset(m *Manager, name string) *cluster.Dataset {
	d, _ := m.cfg.Cluster.View().Dataset(name)
	return d
}

// Sync runs one reconcile pass and then fetches what it left in state fetching, in the
// caller's goroutine, for nodes without the background loops (a `nu follow` run
// without a daemon, tests). Started managers use Kick instead.
func (m *Manager) Sync(ctx context.Context) error {
	if err := m.Reconcile(ctx); err != nil {
		return err
	}
	rows, err := m.db.Query(`SELECT ds, path, entry FROM holdings WHERE state = ? ORDER BY ds, path LIMIT 1024`, StateFetching)
	if err != nil {
		return err
	}
	type job struct {
		ds, path string
		entry    cid.CID
	}
	var jobs []job
	for rows.Next() {
		var j job
		var eb []byte
		if rows.Scan(&j.ds, &j.path, &eb) == nil {
			j.entry, _ = cid.Decode(eb)
			jobs = append(jobs, j)
		}
	}
	rows.Close()
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m.fetchItem(j.ds, j.path, j.entry)
	}
	return nil
}
