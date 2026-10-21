package collection

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/store"
)

// ErrNoWriterKey means this node holds no key allowed to write the collection's head.
var ErrNoWriterKey = errors.New("collection: this node holds no writer key for the collection")

// Put queues a change to this node's writer head of a collection: e is the item's entry
// (an entry with `meta`), or nil to remove the path. Queued changes are durable (they
// survive restarts) and are written into the head at most once per head interval.
// Putting an identical entry again is a no-op once written.
func (m *Manager) Put(name, path string, e *tree.Entry) error {
	segs, err := splitPath(path)
	if err != nil {
		return err
	}
	var eb []byte
	if e != nil {
		if err := e.Validate(); err != nil {
			return err
		}
		if eb, err = drisl.Marshal(e); err != nil {
			return err
		}
	}
	if _, err := m.db.Exec(`INSERT OR REPLACE INTO pending (ds, path, entry) VALUES (?, ?, ?)`, name, joinSegs(segs), eb); err != nil {
		return err
	}
	m.fmu.Lock()
	wait := time.Until(m.lastFlush[name].Add(m.cfg.HeadInterval))
	m.fmu.Unlock()
	if wait <= 0 {
		m.Kick()
	} else {
		time.AfterFunc(wait, m.Kick)
	}
	return nil
}

func joinSegs(segs []string) string {
	out := ""
	for i, s := range segs {
		if i > 0 {
			out += "/"
		}
		out += s
	}
	return out
}

// Flush writes queued changes (and expiry) into this node's writer head now.
func (m *Manager) Flush(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.cfg.Cluster.View().Dataset(name)
	if !ok || d.Collection == nil {
		return fmt.Errorf("collection: no collection %q", name)
	}
	return m.flushHeadsLocked(ctx, name, d, true)
}

// WriterKey returns the key this node writes a collection's head with: a held key listed
// in the dataset's writers (in list order), else a held admin or root key; "" if none.
func (m *Manager) WriterKey(v *cluster.View, d *cluster.Dataset) string {
	held := m.heldKeys()
	for _, w := range d.Writers {
		if k := v.Keys[w]; held[w] && k != nil && k.Revoked == nil {
			return w
		}
	}
	for _, role := range []string{cluster.RoleAdmin, cluster.RoleRoot} {
		for did := range held {
			if k := v.Keys[did]; k != nil && k.Revoked == nil && k.Role == role {
				return did
			}
		}
	}
	return ""
}

type pendingPut struct {
	path  string
	entry []byte
}

func (m *Manager) flushHeadsLocked(ctx context.Context, name string, d *cluster.Dataset, force bool) error {
	rows, err := m.db.Query(`SELECT path, entry FROM pending WHERE ds = ? ORDER BY path`, name)
	if err != nil {
		return err
	}
	var puts []pendingPut
	for rows.Next() {
		var p pendingPut
		if err := rows.Scan(&p.path, &p.entry); err != nil {
			rows.Close()
			return err
		}
		puts = append(puts, p)
	}
	rows.Close()
	expire := d.Collection.Expire
	if len(puts) == 0 && expire == nil {
		return nil
	}
	v := m.cfg.Cluster.View()
	did := m.WriterKey(v, d)
	if did == "" {
		if len(puts) > 0 {
			return ErrNoWriterKey
		}
		return nil
	}
	h, hasHead := v.Heads[name][did]
	if len(puts) == 0 && !hasHead {
		return nil
	}
	m.fmu.Lock()
	last := m.lastFlush[name]
	m.fmu.Unlock()
	if !force && m.now().Sub(last) < m.cfg.HeadInterval {
		return nil // the timer set by Put (or the next pass) flushes later
	}
	var root, oldHeader cid.CID
	var size uint64
	if hasHead && !h.Deleted && h.Value != nil {
		if oldHeader, err = cid.Parse(h.Value.Target); err != nil {
			return err
		}
		b, err := m.cfg.Store.Get(oldHeader)
		if err != nil {
			return fmt.Errorf("own head %s is not held: %w", oldHeader, err)
		}
		hd, err := tree.DecodeHeader(b)
		if err != nil {
			return err
		}
		root, size = hd.Root, hd.Bytes
	}
	ed, err := newEditor(m.cfg.Store, root, size)
	if err != nil {
		return err
	}
	for _, p := range puts {
		segs, err := splitPath(p.path)
		if err != nil {
			continue
		}
		if p.entry == nil {
			if err := ed.del(segs); err != nil {
				return err
			}
			continue
		}
		var e tree.Entry
		if err := drisl.Unmarshal(p.entry, &e); err != nil {
			return err
		}
		if err := ed.put(segs, e); err != nil {
			return err
		}
	}
	if expire != nil {
		after, err := ParseDuration(expire.After)
		if err != nil {
			return fmt.Errorf("expire: %w", err)
		}
		cutoff := m.now().Add(-after).UnixMilli()
		rows, err := m.db.Query(`SELECT path FROM ftime WHERE ds = ? AND facet = ? AND ms < ?`, name, expire.Facet, cutoff)
		if err != nil {
			return err
		}
		var old []string
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				old = append(old, p)
			}
		}
		rows.Close()
		for _, p := range old {
			segs := split(p)
			if st, _, err := ed.lookup(segs); err == nil && st == leaf {
				if err := ed.del(segs); err != nil {
					return err
				}
			}
		}
	}
	newRoot, err := ed.flush()
	if err != nil {
		return err
	}
	clear := func() error {
		for _, p := range puts {
			if _, err := m.db.Exec(`DELETE FROM pending WHERE ds = ? AND path = ? AND entry IS ?`, name, p.path, p.entry); err != nil {
				return err
			}
		}
		return nil
	}
	if newRoot == root && oldHeader.Defined() {
		return clear()
	}
	hdr := tree.Header{V: 1, Root: newRoot, Bytes: ed.root.size, Collection: map[string]any{"facets": facetsGeneric(facetSpecs(d))}}
	hc, hb, err := drisl.Sum(hdr)
	if err != nil {
		return err
	}
	if err := m.cfg.Store.Put(hc, hb); err != nil {
		return err
	}
	// A writer holds everything in its own head: it is the first source of new items.
	pin := store.Pin{CID: hc, Mode: store.Recursive, Profile: "nu-tree", Source: store.SourceCluster, Name: "writer/" + name + "/" + did}
	if err := m.cfg.Store.Pin(pin); err != nil {
		return err
	}
	if _, err := m.cfg.Cluster.WriteAs(did, cluster.Op{Collection: cluster.NSHead, RKey: name,
		Value: &cluster.Head{Target: hc.String(), Kind: "tree", Size: int64(ed.root.size)}}); err != nil {
		return err
	}
	if oldHeader.Defined() && oldHeader != hc {
		pin.CID = oldHeader
		_, _ = m.cfg.Store.Unpin(pin)
	}
	m.fmu.Lock()
	m.lastFlush[name] = m.now()
	m.fmu.Unlock()
	_ = ctx
	return clear()
}
