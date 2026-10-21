package collection

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Holdings serves a `/nu/cluster/1 holdings` request from this node's holdings log.
func (m *Manager) Holdings(_ wire.PeerID, req cluster.HoldingsRequest) (*cluster.HoldingsPage, error) {
	limit := req.Limit
	if limit <= 0 || limit > cluster.MaxHoldingsPage {
		limit = cluster.MaxHoldingsPage
	}
	page := &cluster.HoldingsPage{Epoch: m.epoch, Entries: []cluster.HoldingsEntry{}}
	lo, hi := m.LogMark(req.Dataset)
	if req.Snapshot {
		page.Seq = req.Since
		if req.After == "" || req.Epoch != m.epoch {
			page.Seq = hi
		}
		rows, err := m.db.Query(`SELECT path, entry, state, src, bytes FROM holdings WHERE ds = ? AND path > ? AND state != ? ORDER BY path LIMIT ?`,
			req.Dataset, req.After, StatePaused, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var e cluster.HoldingsEntry
			var eb []byte
			if err := rows.Scan(&e.Path, &eb, &e.State, &e.Src, &e.Bytes); err != nil {
				return nil, err
			}
			e.Entry, _ = cid.Decode(eb)
			e.Seq = page.Seq
			page.Entries = append(page.Entries, e)
		}
		if len(page.Entries) == limit {
			page.More, page.After = true, page.Entries[len(page.Entries)-1].Path
		}
		return page, rows.Err()
	}
	// A cursor from another epoch, past the log head or older than the log's tail
	// answers resync, never an empty delta.
	if req.Epoch != m.epoch || req.Since > hi || (lo > 0 && req.Since < lo-1) {
		page.Resync, page.Seq = true, hi
		return page, nil
	}
	rows, err := m.db.Query(`SELECT seq, path, entry, state, src, bytes FROM hlog WHERE ds = ? AND seq > ? ORDER BY seq LIMIT ?`, req.Dataset, req.Since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page.Seq = req.Since
	for rows.Next() {
		var e cluster.HoldingsEntry
		var eb []byte
		if err := rows.Scan(&e.Seq, &e.Path, &eb, &e.State, &e.Src, &e.Bytes); err != nil {
			return nil, err
		}
		e.Entry, _ = cid.Decode(eb)
		page.Entries = append(page.Entries, e)
		page.Seq = e.Seq
	}
	page.More = len(page.Entries) == limit
	return page, rows.Err()
}

// HoldingsMarks reports this node's holdings-log positions (for heartbeats).
func (m *Manager) HoldingsMarks() []cluster.HoldingsMark {
	var out []cluster.HoldingsMark
	for _, name := range Collections(m.cfg.Cluster.View()) {
		_, hi := m.LogMark(name)
		out = append(out, cluster.HoldingsMark{Dataset: name, Epoch: m.epoch, Seq: hi})
	}
	return out
}

// indexOn reports whether this node runs the holdings index for a collection.
func (m *Manager) indexOn(name string) bool {
	if m.cfg.Index {
		return true
	}
	rules, _ := m.Rules(name)
	for _, r := range rules {
		if r.Rescue != nil {
			return true
		}
	}
	return false
}

// freshness reports, per member, whether its heartbeat is fresh (received within three
// heartbeat intervals); this node is always fresh.
func (m *Manager) freshness() map[string]bool {
	out := map[string]bool{m.cfg.Self.String(): true}
	if m.cfg.Service == nil {
		return out
	}
	svc := m.cfg.Service()
	if svc == nil {
		return out
	}
	now := m.now()
	for id, p := range svc.Peers() {
		out[id.String()] = p.Heartbeat != nil && now.Sub(p.Received) <= 3*m.cfg.Interval
	}
	return out
}

type cursor struct {
	epoch string
	seq   int64
}

func (m *Manager) loadCursor(name, peer string) (cursor, bool) {
	var c cursor
	err := m.db.QueryRow(`SELECT epoch, seq FROM idx_cursor WHERE ds = ? AND peer = ?`, name, peer).Scan(&c.epoch, &c.seq)
	return c, err == nil
}

// pullIndex follows every storage member's holdings log (this node's directly) from the
// persisted cursors, falling back to a snapshot when a cursor is too old.
func (m *Manager) pullIndex(ctx context.Context, name string, v *cluster.View) {
	var svc *cluster.Service
	if m.cfg.Service != nil {
		svc = m.cfg.Service()
	}
	var peers map[wire.PeerID]cluster.PeerStatus
	if svc != nil {
		peers = svc.Peers()
	}
	for _, id := range v.MemberIDs() {
		var fetch func(req cluster.HoldingsRequest) (*cluster.HoldingsPage, error)
		var mark *cluster.HoldingsMark
		if id == m.cfg.Self {
			fetch = func(req cluster.HoldingsRequest) (*cluster.HoldingsPage, error) { return m.Holdings(id, req) }
			_, hi := m.LogMark(name)
			mark = &cluster.HoldingsMark{Dataset: name, Epoch: m.epoch, Seq: hi}
		} else if svc != nil {
			if p, ok := peers[id]; ok && p.Heartbeat != nil {
				for _, hm := range p.Heartbeat.Holdings {
					if hm.Dataset == name {
						mark = &hm
					}
				}
			}
			fetch = func(req cluster.HoldingsRequest) (*cluster.HoldingsPage, error) {
				cctx, cancel := context.WithTimeout(ctx, time.Minute)
				defer cancel()
				return svc.Holdings(cctx, id, req)
			}
		}
		if mark == nil || fetch == nil {
			continue
		}
		cur, ok := m.loadCursor(name, id.String())
		if ok && cur.epoch == mark.Epoch && cur.seq >= mark.Seq {
			continue
		}
		if err := m.follow(name, id.String(), cur, fetch); err != nil && ctx.Err() == nil {
			m.log.Debug("holdings pull failed", "collection", name, "peer", id, "err", err)
		}
	}
}

func (m *Manager) follow(name, peer string, cur cursor, fetch func(cluster.HoldingsRequest) (*cluster.HoldingsPage, error)) error {
	for range 1000 {
		page, err := fetch(cluster.HoldingsRequest{Dataset: name, Epoch: cur.epoch, Since: cur.seq})
		if err != nil {
			return err
		}
		if page.Resync {
			if cur, err = m.snapshot(name, peer, fetch); err != nil {
				return err
			}
			continue
		}
		tx, err := m.db.Begin()
		if err != nil {
			return err
		}
		for _, e := range page.Entries {
			if err := applyIndex(tx, name, peer, e); err != nil {
				tx.Rollback()
				return err
			}
		}
		cur = cursor{page.Epoch, page.Seq}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO idx_cursor (ds, peer, epoch, seq) VALUES (?, ?, ?, ?)`, name, peer, cur.epoch, cur.seq); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if !page.More {
			return nil
		}
	}
	return nil
}

func applyIndex(tx *sql.Tx, name, peer string, e cluster.HoldingsEntry) error {
	if e.State == StateReleased {
		_, err := tx.Exec(`DELETE FROM idx WHERE ds = ? AND peer = ? AND path = ?`, name, peer, e.Path)
		return err
	}
	_, err := tx.Exec(`INSERT OR REPLACE INTO idx (ds, peer, path, entry, state, src, bytes) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, peer, e.Path, cidBytes(e.Entry), e.State, e.Src, e.Bytes)
	return err
}

// snapshot replaces a member's index rows with a paged snapshot of its holdings and
// returns the log cursor the snapshot started at.
func (m *Manager) snapshot(name, peer string, fetch func(cluster.HoldingsRequest) (*cluster.HoldingsPage, error)) (cursor, error) {
	var all []cluster.HoldingsEntry
	req := cluster.HoldingsRequest{Dataset: name, Snapshot: true}
	var cur cursor
	for {
		page, err := fetch(req)
		if err != nil {
			return cur, err
		}
		if req.After == "" {
			cur = cursor{page.Epoch, page.Seq}
		} else if page.Epoch != cur.epoch {
			return cur, errors.New("holdings log epoch changed during a snapshot")
		}
		all = append(all, page.Entries...)
		if !page.More {
			break
		}
		req.After, req.Epoch, req.Since = page.After, cur.epoch, cur.seq
	}
	tx, err := m.db.Begin()
	if err != nil {
		return cur, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM idx WHERE ds = ? AND peer = ?`, name, peer); err != nil {
		return cur, err
	}
	for _, e := range all {
		if err := applyIndex(tx, name, peer, e); err != nil {
			return cur, err
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO idx_cursor (ds, peer, epoch, seq) VALUES (?, ?, ?, ?)`, name, peer, cur.epoch, cur.seq); err != nil {
		return cur, err
	}
	return cur, tx.Commit()
}

// copies counts, per item path, the complete holders with fresh heartbeats (and the
// stale ones as unknown), from the holdings index.
func (m *Manager) copies(name string, fresh map[string]bool, paths []string) (map[string]int, map[string]int, error) {
	q := `SELECT path, peer FROM idx WHERE ds = ? AND state = ?`
	args := []any{name, StateComplete}
	if paths != nil {
		if len(paths) == 0 {
			return map[string]int{}, map[string]int{}, nil
		}
		if len(paths) == 1 {
			q += ` AND path = ?`
			args = append(args, paths[0])
		}
	}
	rows, err := m.db.Query(q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	ok, unknown := map[string]int{}, map[string]int{}
	for rows.Next() {
		var p, peer string
		if err := rows.Scan(&p, &peer); err != nil {
			return nil, nil, err
		}
		if fresh[peer] {
			ok[p]++
		} else {
			unknown[p]++
		}
	}
	return ok, unknown, rows.Err()
}

// rescueScore orders rescue holders of an item for release damping.
func rescueScore(peer, path string) [32]byte {
	return sha256.Sum256([]byte("nu.rescue\x00" + peer + "\x00" + path))
}

// rescue runs the rescue rules: hold matched items whose fresh complete-holder count is
// below `below`, rarest first, within budget; release rescued items once their count
// reaches below+1, one rescuer at a time (the fresh rescue holder with the greatest
// score), so concurrent rescuers acting on the same counts do not all step back.
func (m *Manager) rescue(name string) error {
	rules, err := m.Rules(name)
	if err != nil {
		return err
	}
	fresh := m.freshness()
	counts, _, err := m.copies(name, fresh, nil)
	if err != nil {
		return err
	}
	self := m.cfg.Self.String()
	released := false
	for i, r := range rules {
		if r.Rescue == nil {
			continue
		}
		below := int(r.Rescue.Below)
		// Releases first, to make room.
		rows, err := m.db.Query(`SELECT path FROM holdings WHERE ds = ? AND src = ? AND rule = ? AND state = ?`, name, SrcRescue, i, StateComplete)
		if err != nil {
			return err
		}
		var mine []string
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				mine = append(mine, p)
			}
		}
		rows.Close()
		for _, p := range mine {
			if counts[p] < below+1 || !m.designatedReleaser(name, p, self, fresh) {
				continue
			}
			cur, err := m.holding(name, p)
			if err != nil {
				return err
			}
			if r, err := m.release(name, p, cur); err != nil {
				return err
			} else if r {
				released = true
				counts[p]--
			}
		}
		// Acquisitions, rarest first.
		rows, err = m.db.Query(`SELECT `+itemCols+` FROM items WHERE ds = ? AND rule = ? AND deleted = 0
			AND path NOT IN (SELECT path FROM holdings WHERE ds = ? AND state != ?)`, name, i, name, StatePaused)
		if err != nil {
			return err
		}
		var cands []*itemRow
		for rows.Next() {
			it, err := scanItem(rows)
			if err != nil {
				rows.Close()
				return err
			}
			if counts[it.path] < below {
				cands = append(cands, it)
			}
		}
		rows.Close()
		sort.SliceStable(cands, func(a, b int) bool {
			if counts[cands[a].path] != counts[cands[b].path] {
				return counts[cands[a].path] < counts[cands[b].path]
			}
			return cands[a].path < cands[b].path
		})
		for _, it := range cands {
			cur, err := m.holding(name, it.path)
			if err != nil {
				return err
			}
			if err := m.hold(name, it, cur, want{SrcRescue, i}, rules); err != nil {
				return err
			}
			if h, _ := m.holding(name, it.path); h != nil && h.state == StatePaused {
				_, _ = m.db.Exec(`DELETE FROM holdings WHERE ds = ? AND path = ?`, name, it.path)
				break // budget exhausted; rarer items were taken first
			}
		}
	}
	if released {
		m.scheduleGC()
	}
	return nil
}

func (m *Manager) designatedReleaser(name, path, self string, fresh map[string]bool) bool {
	rows, err := m.db.Query(`SELECT peer FROM idx WHERE ds = ? AND path = ? AND state = ? AND src = ?`, name, path, StateComplete, SrcRescue)
	if err != nil {
		return false
	}
	defer rows.Close()
	var holders []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil && fresh[p] {
			holders = append(holders, p)
		}
	}
	if !slices.Contains(holders, self) {
		holders = append(holders, self)
	}
	best := self
	bs := rescueScore(self, path)
	for _, h := range holders {
		if s := rescueScore(h, path); string(s[:]) > string(bs[:]) {
			best, bs = h, s
		}
	}
	return best == self
}
