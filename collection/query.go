package collection

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
)

// Rules returns this node's selection rules for a collection.
func (m *Manager) Rules(name string) ([]Rule, error) {
	var j string
	err := m.db.QueryRow(`SELECT json FROM rules WHERE ds = ?`, name).Scan(&j)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rules []Rule
	if err := json.Unmarshal([]byte(j), &rules); err != nil {
		return nil, fmt.Errorf("collection: stored rules: %w", err)
	}
	return rules, nil
}

func (m *Manager) dataset(name string) (map[string]FacetSpec, error) {
	d, ok := m.cfg.Cluster.View().Dataset(name)
	if !ok || d.Collection == nil {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "no collection %q in the cluster", name).
			WithAction("list datasets with `nu cluster dataset ls`; collections are created with `nu cluster dataset create <name> --collection`")
	}
	return m.specs(name, d), nil
}

// SetRules validates rules against the collection's facets and reports what each would
// hold now; unless dryRun, it stores them, and selection re-evaluates every item. A
// followed collection refuses rescue rules, and its rules are checked against the
// facets only once its catalog has been synced (they are ignored, with a status
// warning, while they do not fit).
func (m *Manager) SetRules(name string, rules []Rule, dryRun bool) (*apiv0.CollectionRules, error) {
	specs, err := m.dataset(name)
	if err != nil {
		return nil, err
	}
	if len(rules) > 256 {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "at most 256 rules")
	}
	d, _ := m.cfg.Cluster.View().Dataset(name)
	check := true
	if IsFollow(d) {
		if err := checkFollowRules(rules); err != nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err).WithAction("rescue needs a cluster; follow with plain selection rules")
		}
		if row, err := m.loadColl(name); err == nil && len(row.writers) == 0 {
			check = false // no catalog synced yet: the facets are unknown
		}
	}
	if err := Compile(rules, specs); check && err != nil {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err)
	}
	if rules == nil {
		rules = []Rule{}
	}
	j, err := json.Marshal(rules)
	if err != nil {
		return nil, err
	}
	out := &apiv0.CollectionRules{Rules: j, Applied: !dryRun}
	if out.DryRun, err = m.impact(name, rules); err != nil {
		return nil, err
	}
	if dryRun {
		return out, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.loadColl(name); err != nil {
		return nil, err
	}
	if _, err := m.db.Exec(`INSERT OR REPLACE INTO rules (ds, json) VALUES (?, ?)`, name, string(j)); err != nil {
		return nil, err
	}
	if _, err := m.db.Exec(`UPDATE coll SET rules_gen = rules_gen + 1 WHERE ds = ?`, name); err != nil {
		return nil, err
	}
	m.Kick()
	return out, nil
}

// impact computes, per rule, the items it would decide now (first match) and what its
// budget leaves out, taking items in path order.
func (m *Manager) impact(name string, rules []Rule) ([]apiv0.RuleImpact, error) {
	out := make([]apiv0.RuleImpact, len(rules))
	for i, r := range rules {
		out[i] = apiv0.RuleImpact{Rule: i, Rescue: r.Rescue != nil, Hold: r.Holds(), BudgetBytes: r.Budget()}
	}
	rows, err := m.db.Query(`SELECT `+itemCols+` FROM items WHERE ds = ? AND deleted = 0 ORDER BY path`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := m.now()
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		i := Select(rules, it.path, it.facets, now)
		if i < 0 {
			continue
		}
		ri := &out[i]
		if b := rules[i].Budget(); b > 0 && int64(ri.Bytes+it.size) > b {
			ri.PausedItems++
			ri.PausedBytes += it.size
			continue
		}
		ri.Items++
		ri.Bytes += it.size
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hr, err := m.db.Query(`SELECT rule, COUNT(*) FROM holdings WHERE ds = ? AND state != ? AND rule >= 0 GROUP BY rule`, name, StatePaused)
	if err != nil {
		return nil, err
	}
	defer hr.Close()
	for hr.Next() {
		var r, n int
		if hr.Scan(&r, &n) == nil && r < len(out) {
			out[r].CurrentItems = n
		}
	}
	return out, hr.Err()
}

// Hold sets a manual hold on an item (pin source collection-manual); Release clears it.
func (m *Manager) Hold(name, path string) error    { return m.manual(name, path, true) }
func (m *Manager) Release(name, path string) error { return m.manual(name, path, false) }

func (m *Manager) manual(name, path string, on bool) error {
	segs, err := splitPath(path)
	if err != nil {
		return apiv0.Errorf(apiv0.CodeInvalidArgument, "%v", err)
	}
	path = joinSegs(segs)
	if _, err := m.dataset(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	it, err := m.item(name, path)
	if err != nil {
		return err
	}
	if on && (it == nil || it.deleted) {
		return apiv0.Errorf(apiv0.CodeNotFound, "no item %q in collection %q", path, name)
	}
	if on {
		_, err = m.db.Exec(`INSERT OR IGNORE INTO manual (ds, path) VALUES (?, ?)`, name, path)
	} else {
		_, err = m.db.Exec(`DELETE FROM manual WHERE ds = ? AND path = ?`, name, path)
	}
	if err != nil {
		return err
	}
	rules, _ := m.Rules(name)
	specs, _ := m.dataset(name)
	if Compile(rules, specs) != nil {
		rules = nil
	}
	d, _ := m.cfg.Cluster.View().Dataset(name)
	own, err := m.ownHeads(name, d)
	if err != nil {
		return err
	}
	released, err := m.evalItem(name, path, rules, m.now(), own, m.floor(d), followLimit(d))
	if released {
		m.scheduleGC()
	}
	return err
}

func (m *Manager) itemInfo(it *itemRow) apiv0.CollectionItem {
	out := apiv0.CollectionItem{Path: it.path, Seq: it.seq, Removed: it.deleted, Rule: it.rule}
	if it.deleted {
		return out
	}
	out.Entry, out.Kind, out.Size, out.Writer = it.entry.String(), it.kind, it.size, it.writer
	if it.meta.Defined() {
		out.Meta = it.meta.String()
	}
	if len(it.facets) > 0 {
		out.Facets = map[string]any{}
		for k, v := range it.facets {
			out.Facets[k] = v.JSON()
		}
	}
	return out
}

// Items answers an items query: the change stream after a cursor, or the current items
// matching filters (rarest first, by holder, unheld, held here, path prefix, facet
// predicates).
func (m *Manager) Items(name string, q apiv0.CollectionItemsQuery) (*apiv0.Page[apiv0.CollectionItem], error) {
	if _, err := m.dataset(name); err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = apiv0.DefaultPageLimit
	}
	limit = min(limit, apiv0.MaxPageLimit)
	if q.Since != "" {
		return m.stream(name, q.Since, limit)
	}
	var match *Rule
	if len(q.Match) > 0 {
		var r Rule
		if err := json.Unmarshal(q.Match, &r); err != nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "bad match: %v", err)
		}
		specs, _ := m.dataset(name)
		if err := Compile([]Rule{r}, specs); err != nil {
			return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "bad match: %v", err)
		}
		match = &r
	}
	// The filters and ordering need only paths (and facets for a match); full rows are
	// read for the page alone.
	cols := `path, NULL`
	if match != nil {
		cols = `path, facets`
	}
	sqlq := `SELECT ` + cols + ` FROM items WHERE ds = ? AND deleted = 0`
	args := []any{name}
	if q.Prefix != "" {
		p := strings.Trim(q.Prefix, "/")
		sqlq += ` AND (path = ? OR substr(path, 1, ?) = ?)`
		args = append(args, p, len(p)+1, p+"/")
	}
	if q.Held {
		sqlq += ` AND path IN (SELECT path FROM holdings WHERE ds = ? AND state != ?)`
		args = append(args, name, StatePaused)
	}
	if q.Holder != "" {
		peer, err := parsePeer(q.Holder)
		if err != nil {
			return nil, err
		}
		sqlq += ` AND path IN (SELECT path FROM idx WHERE ds = ? AND peer = ?)`
		args = append(args, name, peer)
	}
	rows, err := m.db.Query(sqlq+` ORDER BY path`, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	now := m.now()
	for rows.Next() {
		var p string
		var fj sql.NullString
		if err := rows.Scan(&p, &fj); err != nil {
			rows.Close()
			return nil, err
		}
		if match != nil {
			var f map[string]Value
			if fj.Valid {
				_ = json.Unmarshal([]byte(fj.String), &f)
			}
			if !match.Match(p, f, now) {
				continue
			}
		}
		paths = append(paths, p)
	}
	rows.Close()
	index := m.indexOn(name)
	if (q.Rarest || q.Unheld) && !index {
		return nil, apiv0.Errorf(apiv0.CodeUnavailable, "copy counts need the holdings index, which this node does not run").
			WithAction("query a member running the index (cluster.index), or enable it here")
	}
	var counts, unknown map[string]int
	if index {
		if counts, unknown, err = m.copies(name, m.freshness(), nil); err != nil {
			return nil, err
		}
	}
	if q.Unheld {
		paths = slices.DeleteFunc(paths, func(p string) bool { return counts[p] > 0 })
	}
	if q.Rarest {
		slices.SortStableFunc(paths, func(a, b string) int { return counts[a] - counts[b] })
	}
	page, err := apiv0.PageSlice(paths, q.Cursor, limit, m.epoch)
	if err != nil {
		return nil, err
	}
	out := &apiv0.Page[apiv0.CollectionItem]{Items: make([]apiv0.CollectionItem, 0, len(page.Items)), NextCursor: page.NextCursor, Resync: page.Resync}
	items, local, err := m.pageRows(name, page.Items)
	if err != nil {
		return nil, err
	}
	var holders map[string][]apiv0.CollectionHolder
	if q.Holders && index {
		if holders, err = m.pageHolders(name, page.Items); err != nil {
			return nil, err
		}
	}
	for _, p := range page.Items {
		it := items[p]
		if it == nil {
			continue
		}
		ci := m.itemInfo(it)
		ci.Local = local[p]
		ci.Holders = holders[p]
		if index {
			c, u := counts[p], unknown[p]
			ci.Copies, ci.Unknown = &c, &u
		}
		out.Items = append(out.Items, ci)
	}
	return out, nil
}

// pageRows reads the items and local holding states of a page of paths, a few hundred
// paths per query.
func (m *Manager) pageRows(name string, paths []string) (map[string]*itemRow, map[string]string, error) {
	items, local := map[string]*itemRow{}, map[string]string{}
	for lo := 0; lo < len(paths); lo += 500 {
		chunk := paths[lo:min(lo+500, len(paths))]
		in := strings.Repeat(",?", len(chunk))[1:]
		args := []any{name}
		for _, p := range chunk {
			args = append(args, p)
		}
		rows, err := m.db.Query(`SELECT `+itemCols+` FROM items WHERE ds = ? AND path IN (`+in+`)`, args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			it, err := scanItem(rows)
			if err != nil {
				rows.Close()
				return nil, nil, err
			}
			items[it.path] = it
		}
		rows.Close()
		rows, err = m.db.Query(`SELECT path, state FROM holdings WHERE ds = ? AND path IN (`+in+`)`, args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var p, st string
			if err := rows.Scan(&p, &st); err != nil {
				rows.Close()
				return nil, nil, err
			}
			local[p] = st
		}
		rows.Close()
	}
	return items, local, nil
}

// pageHolders reads the holders the index reports for a page of paths.
func (m *Manager) pageHolders(name string, paths []string) (map[string][]apiv0.CollectionHolder, error) {
	fresh := m.freshness()
	self := m.cfg.Self.String()
	out := map[string][]apiv0.CollectionHolder{}
	for lo := 0; lo < len(paths); lo += 500 {
		chunk := paths[lo:min(lo+500, len(paths))]
		args := []any{name}
		for _, p := range chunk {
			args = append(args, p)
		}
		rows, err := m.db.Query(`SELECT path, peer, state, src FROM idx WHERE ds = ? AND path IN (`+strings.Repeat(",?", len(chunk))[1:]+`) ORDER BY path, peer`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p, peer string
			var h apiv0.CollectionHolder
			if err := rows.Scan(&p, &peer, &h.State, &h.Src); err != nil {
				rows.Close()
				return nil, err
			}
			h.Peer, h.Fresh, h.Self = base36(peer), fresh[peer], peer == self
			out[p] = append(out[p], h)
		}
		rows.Close()
	}
	return out, nil
}

func parsePeer(s string) (string, error) {
	id, err := parsePeerID(s)
	if err != nil {
		return "", apiv0.Errorf(apiv0.CodeInvalidArgument, "bad PeerId %q: %v", s, err)
	}
	return id, nil
}

func (m *Manager) localStates(name string) (map[string]string, error) {
	rows, err := m.db.Query(`SELECT path, state FROM holdings WHERE ds = ?`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, s string
		if err := rows.Scan(&p, &s); err != nil {
			return nil, err
		}
		out[p] = s
	}
	return out, rows.Err()
}

// stream returns item changes after a cursor ("0": from the start). Each changed path
// appears once with its latest state; removals are marked. next_cursor is always set.
func (m *Manager) stream(name, since string, limit int) (*apiv0.Page[apiv0.CollectionItem], error) {
	var seq int64
	if since != "0" {
		c, err := apiv0.ParseCursor(since)
		if err != nil {
			return nil, err
		}
		if c.Epoch != m.epoch {
			return &apiv0.Page[apiv0.CollectionItem]{Items: []apiv0.CollectionItem{}, Resync: true}, nil
		}
		seq = int64(c.Offset)
	}
	rows, err := m.db.Query(`SELECT `+itemCols+` FROM items WHERE ds = ? AND seq > ? ORDER BY seq LIMIT ?`, name, seq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &apiv0.Page[apiv0.CollectionItem]{Items: []apiv0.CollectionItem{}}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, m.itemInfo(it))
		seq = it.seq
	}
	page.NextCursor = apiv0.Cursor{Epoch: m.epoch, Offset: int(seq)}.Encode()
	return page, rows.Err()
}

// Item describes one item with its holders (from the index, plus this node) and conflicts.
func (m *Manager) Item(name, path string) (*apiv0.CollectionItem, error) {
	if _, err := m.dataset(name); err != nil {
		return nil, err
	}
	path = strings.Trim(path, "/")
	it, err := m.item(name, path)
	if err != nil {
		return nil, err
	}
	push, err := m.PushStatus(name, path)
	if err != nil {
		return nil, err
	}
	if it == nil {
		if push != nil { // pushed to this node, not in the merged catalog (yet)
			return &apiv0.CollectionItem{Path: path, Rule: -1, Push: push}, nil
		}
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "no item %q in collection %q", path, name).
			WithAction("list items with `nu cluster collection items " + name + "`")
	}
	out := m.itemInfo(it)
	out.Push = push
	if d, ok := m.cfg.Cluster.View().Dataset(name); ok && !it.deleted {
		out.Owners = m.floorOwners(d, it.entry)
	}
	if h, err := m.holding(name, path); err == nil && h != nil {
		out.Local = h.state
	}
	fresh := m.freshness()
	self := m.cfg.Self.String()
	seenSelf := false
	rows, err := m.db.Query(`SELECT peer, state, src FROM idx WHERE ds = ? AND path = ? ORDER BY peer`, name, path)
	if err != nil {
		return nil, err
	}
	copies, unknown := 0, 0
	for rows.Next() {
		var h apiv0.CollectionHolder
		var peer string
		if err := rows.Scan(&peer, &h.State, &h.Src); err != nil {
			rows.Close()
			return nil, err
		}
		h.Peer, h.Fresh, h.Self = base36(peer), fresh[peer], peer == self
		seenSelf = seenSelf || h.Self
		if h.State == StateComplete {
			if h.Fresh {
				copies++
			} else {
				unknown++
			}
		}
		out.Holders = append(out.Holders, h)
	}
	rows.Close()
	if !seenSelf {
		if h, _ := m.holding(name, path); held(h) {
			out.Holders = append(out.Holders, apiv0.CollectionHolder{Peer: base36(self), State: h.state, Src: h.src, Fresh: true, Self: true})
		}
	}
	if m.indexOn(name) {
		out.Copies, out.Unknown = &copies, &unknown
	}
	cs, err := m.conflicts(name, path)
	if err != nil {
		return nil, err
	}
	out.Conflicts = cs
	return &out, nil
}

// Conflicts lists the union-merge conflicts of a collection.
func (m *Manager) Conflicts(name string) ([]apiv0.CollectionConflict, error) {
	if _, err := m.dataset(name); err != nil {
		return nil, err
	}
	return m.conflicts(name, "")
}

func (m *Manager) conflicts(name, path string) ([]apiv0.CollectionConflict, error) {
	q, args := `SELECT path, loser, winner, entry, reason FROM conflicts WHERE ds = ?`, []any{name}
	if path != "" {
		q += ` AND path = ?`
		args = append(args, path)
	}
	rows, err := m.db.Query(q+` ORDER BY path, loser`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []apiv0.CollectionConflict{}
	for rows.Next() {
		var c apiv0.CollectionConflict
		var w sql.NullString
		var eb []byte
		if err := rows.Scan(&c.Path, &c.Loser, &w, &eb, &c.Reason); err != nil {
			return nil, err
		}
		c.Winner = w.String
		e, _ := cid.Decode(eb)
		c.Entry = e.String()
		out = append(out, c)
	}
	return out, rows.Err()
}

// Status summarises a collection on this node.
func (m *Manager) Status(name string) (*apiv0.CollectionStatus, error) {
	specs, err := m.dataset(name)
	if err != nil {
		return nil, err
	}
	v := m.cfg.Cluster.View()
	d, _ := v.Dataset(name)
	row, err := m.loadColl(name)
	if err != nil {
		return nil, err
	}
	st := &apiv0.CollectionStatus{Name: name, Merge: d.Collection.MergeMode(), Replicas: d.Policy.Replicas, Bytes: row.size,
		Writers: []apiv0.CollectionWriter{}, Local: map[string]int{}, LogEpoch: m.epoch}
	if row.header.Defined() {
		st.Root = row.header.String()
	}
	if row.synced > 0 {
		st.Synced = msTime(row.synced)
	}
	merged := map[string]cid.CID{}
	for _, w := range row.writers {
		merged[w.DID] = w.Head
	}
	for _, did := range writerDIDs(v, name, d) {
		h := v.Heads[name][did].Value.Target
		st.Writers = append(st.Writers, apiv0.CollectionWriter{DID: did, Head: h, Synced: merged[did].String() == h})
	}
	// Facet names are plain (no characters JSON escapes) and a quote inside a JSON string
	// is always escaped, so `"name":` occurs in an item's facets JSON exactly when the
	// facet is present: the count needs no JSON decoding.
	names := slices.Sorted(maps.Keys(specs))
	q := `SELECT COUNT(*)`
	args := []any{}
	for _, k := range names {
		q += `, COALESCE(SUM(COALESCE(instr(facets, ?), 0) = 0), 0)`
		args = append(args, `"`+k+`":`)
	}
	absent := make([]int, len(names))
	dest := []any{&st.Items}
	for i := range absent {
		dest = append(dest, &absent[i])
	}
	if err := m.db.QueryRow(q+` FROM items WHERE ds = ? AND deleted = 0`, append(args, name)...).Scan(dest...); err != nil {
		return nil, err
	}
	if len(specs) > 0 {
		st.FacetAbsent = map[string]int{}
		for i, k := range names {
			st.FacetAbsent[k] = absent[i]
		}
	}
	_ = m.db.QueryRow(`SELECT COUNT(*) FROM conflicts WHERE ds = ?`, name).Scan(&st.Conflicts)
	if rules, err := m.Rules(name); err == nil {
		st.Rules = len(rules)
		if err := Compile(rules, specs); err != nil {
			st.Warnings = append(st.Warnings, "local rules are ignored: "+err.Error())
		}
	}
	hr, err := m.db.Query(`SELECT state, COUNT(*), COALESCE(SUM(bytes), 0) FROM holdings WHERE ds = ? GROUP BY state`, name)
	if err != nil {
		return nil, err
	}
	for hr.Next() {
		var s string
		var n int
		var b uint64
		if hr.Scan(&s, &n, &b) == nil {
			st.Local[s] = n
			if s == StatePaused {
				st.Shortfall += b
			} else {
				st.LocalBytes += b
			}
		}
	}
	hr.Close()
	_, st.LogSeq = m.LogMark(name)
	var pending int
	_ = m.db.QueryRow(`SELECT COUNT(*) FROM pending WHERE ds = ?`, name).Scan(&pending)
	if pending > 0 && m.WriterKey(v, d) == "" {
		st.Warnings = append(st.Warnings, strconv.Itoa(pending)+" queued item writes, but this node holds no writer key for the collection")
	}
	if st.Shortfall > 0 {
		st.Warnings = append(st.Warnings, "rule budgets (max_bytes) leave matched items unheld")
	}
	st.Floor = m.floorStatus(name, d)
	if m.indexOn(name) {
		if st.Index, err = m.indexStatus(name, v.MemberIDs()); err != nil {
			return nil, err
		}
	}
	return st, nil
}
