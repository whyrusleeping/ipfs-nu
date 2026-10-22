package collection

import (
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
)

// Floor placement (docs/collections.md "Guaranteed copies"): with replicas > 0 (or all),
// every item is one placement unit, owned by the members that rendezvous hashing
// (docs/cluster.md "Placement") ranks highest for the multihash of the item root (its
// entry's CID). Owners hold the whole item; readiness counts, per item, the owners whose
// fresh completion report (mode owned, unit item) confirms it.

// ItemUnit is an item's placement unit: the multihash of its root.
func ItemUnit(entry cid.CID) cid.Multihash { return entry.Multihash() }

// floor returns a collection's floor placement, or nil without guaranteed copies.
func (m *Manager) floor(d *cluster.Dataset) *cluster.Placement {
	if d == nil || (d.Policy.Replicas <= 0 && !d.Policy.All) {
		return nil
	}
	return cluster.NewPlacement(m.cfg.Cluster.View(), d.Policy, m.cfg.Self)
}

// toSurplus keeps a complete floor holding whose item this member no longer owns (or
// that left the catalog) as surplus.
func (m *Manager) toSurplus(name, path string, cur *holding) error {
	if cur.src == SrcSurplus {
		return nil
	}
	if err := m.cfg.Store.Pin(itemPin(name, path, SrcSurplus, cur.entry)); err != nil {
		return err
	}
	_, _ = m.cfg.Store.Unpin(itemPin(name, path, cur.src, cur.entry))
	if _, err := m.db.Exec(`UPDATE holdings SET src = ? WHERE ds = ? AND path = ?`, SrcSurplus, name, path); err != nil {
		return err
	}
	return m.appendLog(name, path, cur.entry, cur.state, SrcSurplus, cur.bytes)
}

// updateReport recomputes this node's floor completion report of a collection (m.mu
// held): bound to the merged root, the policy and the placement epoch; complete when the
// last evaluation succeeded and every floor holding is complete. Owned counts items.
func (m *Manager) updateReport(name string, evalOK bool) {
	v := m.cfg.Cluster.View()
	d, _ := v.Dataset(name)
	fl := m.floor(d)
	var rep cluster.Report
	ok := false
	if fl != nil && fl.Eligible(m.cfg.Self) {
		if row, err := m.loadColl(name); err == nil && row.header.Defined() {
			ok = true
			rep = cluster.Report{Name: name, Target: row.header.String(), Mode: cluster.ModeOwned, Policy: d.PolicyHash(), Epoch: fl.Epoch}
			var missing cluster.Counts
			rows, err := m.db.Query(`SELECT state, COUNT(*), COALESCE(SUM(bytes), 0) FROM holdings WHERE ds = ? AND src = ? GROUP BY state`, name, SrcFloor)
			if err == nil {
				for rows.Next() {
					var st string
					var n, b int64
					if rows.Scan(&st, &n, &b) != nil {
						continue
					}
					rep.Owned.Blobs += n
					rep.Owned.Bytes += b
					if st != StateComplete {
						missing.Blobs += n
						missing.Bytes += b
					}
				}
				rows.Close()
			}
			rep.Complete = evalOK && err == nil && missing.Blobs == 0
			switch {
			case rep.Complete:
				rep.State = cluster.SyncHeld
			case err != nil || !evalOK:
				rep.State = cluster.SyncError
			default:
				rep.State = cluster.SyncSyncing
			}
			if !rep.Complete {
				rep.Missing = &missing
			}
		}
	}
	m.repMu.Lock()
	old, had := m.reports[name]
	if ok {
		m.reports[name] = rep
	} else {
		delete(m.reports, name)
	}
	m.repMu.Unlock()
	if (had != ok || (ok && !old.Same(rep))) && m.cfg.Changed != nil {
		m.cfg.Changed()
	}
}

// dropReports forgets reports of collections that are gone.
func (m *Manager) dropReports(v *cluster.View) {
	m.repMu.Lock()
	defer m.repMu.Unlock()
	for name := range m.reports {
		if d, ok := v.Dataset(name); !ok || d.Collection == nil {
			delete(m.reports, name)
		}
	}
}

// Reports returns this node's floor completion reports (seq unset), for heartbeats.
func (m *Manager) Reports() []cluster.Report {
	m.repMu.Lock()
	defer m.repMu.Unlock()
	out := make([]cluster.Report, 0, len(m.reports))
	for _, r := range m.reports {
		out = append(out, r)
	}
	return out
}

// Report returns this node's floor completion report of a collection.
func (m *Manager) Report(name string) (cluster.Report, bool) {
	m.repMu.Lock()
	defer m.repMu.Unlock()
	r, ok := m.reports[name]
	return r, ok
}

// FloorGroups returns the merged root of a collection and its items by floor owners under
// pl (E6 with items as units; an empty catalog counts the root itself). ok is false
// before a merged catalog is held.
func (m *Manager) FloorGroups(name string, pl *cluster.Placement) (root cid.CID, groups []cluster.OwnerGroup, ok bool) {
	row, err := m.loadColl(name)
	if err != nil || !row.header.Defined() {
		return cid.CID{}, nil, false
	}
	rows, err := m.db.Query(`SELECT entry FROM items WHERE ds = ? AND deleted = 0`, name)
	if err != nil {
		return row.header, nil, false
	}
	defer rows.Close()
	var gs cluster.GroupSet
	n := 0
	for rows.Next() {
		var eb []byte
		if rows.Scan(&eb) != nil {
			return row.header, nil, false
		}
		e, err := cid.Decode(eb)
		if err != nil {
			continue
		}
		gs.Add(pl.Owners(ItemUnit(e)), 1)
		n++
	}
	if rows.Err() != nil {
		return row.header, nil, false
	}
	if n == 0 {
		gs.Add(pl.Owners(row.header.Multihash()), 1)
	}
	return row.header, gs.List(), true
}

// MergedRoot returns the merged catalog header of a collection, if held.
func (m *Manager) MergedRoot(name string) (cid.CID, bool) {
	row, err := m.loadColl(name)
	if err != nil || !row.header.Defined() {
		return cid.CID{}, false
	}
	return row.header, true
}

// floorStatus summarises this node's floor holdings of a collection.
func (m *Manager) floorStatus(name string, d *cluster.Dataset) *apiv0.CollectionFloor {
	fl := m.floor(d)
	if fl == nil {
		return nil
	}
	out := &apiv0.CollectionFloor{Eligible: fl.Eligible(m.cfg.Self), Epoch: fl.Epoch}
	rows, err := m.db.Query(`SELECT src, state, COUNT(*), COALESCE(SUM(bytes), 0) FROM holdings WHERE ds = ? AND src IN (?, ?) GROUP BY src, state`, name, SrcFloor, SrcSurplus)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var src, st string
		var n int
		var b uint64
		if rows.Scan(&src, &st, &n, &b) != nil {
			continue
		}
		switch {
		case src == SrcSurplus:
			out.Surplus += n
		case st == StateComplete:
			out.Owned += n
			out.Complete += n
			out.Bytes += b
		default:
			out.Owned += n
			out.Bytes += b
		}
	}
	return out
}

// floorOwners lists an item's floor owners (base36), nil without guaranteed copies.
func (m *Manager) floorOwners(d *cluster.Dataset, entry cid.CID) []string {
	fl := m.floor(d)
	if fl == nil || !entry.Defined() {
		return nil
	}
	out := []string{}
	for _, id := range fl.Owners(ItemUnit(entry)) {
		out = append(out, id.Base36())
	}
	return out
}
