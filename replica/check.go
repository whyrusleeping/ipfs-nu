package replica

import (
	"fmt"
	"strings"
	"sync"

	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Completeness checks (0078). A check used to walk the whole target with one store
// lookup per object on every reconcile pass and every readiness evaluation, so a
// 2,000-file tree cost seconds per pass. Now a structure walk runs level by level with
// batched lookups (store.HasMany/GetMany) and is memoized per root: the structure of a
// root (its blobs and their sizes) is immutable, and "held" facts stay true until the
// store removes something (store.Removals). A check then only re-queries the blobs not
// yet known held, in batches.

// memoMaxBlobs bounds the blobs remembered over all memoized roots.
const memoMaxBlobs = 1 << 22

// treeMemo is the structure walk of one root under one profile, with the blobs known
// held (complete, copied into the store) as of removals.
type treeMemo struct {
	removals uint64
	blobs    []cid.Multihash // deduplicated, in walk order; never modified

	mu    sync.Mutex
	sizes []uint64 // held size, or the declared link size while missing
	held  []bool
	// witness is a blob last found missing (or -1): rechecked first, it answers "not
	// everything is held" with one lookup.
	witness int
	// owners are the blobs' owners under the placement plKey names.
	plKey  string
	owners [][]wire.PeerID
}

// placementKey identifies what Placement.Owners depends on: the epoch (members, weights,
// tags, states) and the placement fields of the policy.
func placementKey(pl *cluster.Placement) string {
	p := pl.Policy
	return fmt.Sprint(pl.Epoch, "|", p.Replicas, "|", p.All, "|", p.Where, "|", p.Spread, "|", len(pl.Members))
}

// ownersFor returns the owners of every blob under pl (computed once per placement).
func (tm *treeMemo) ownersFor(pl *cluster.Placement) [][]wire.PeerID {
	key := placementKey(pl)
	tm.mu.Lock()
	if tm.plKey == key && tm.owners != nil {
		o := tm.owners
		tm.mu.Unlock()
		return o
	}
	tm.mu.Unlock()
	owners := make([][]wire.PeerID, len(tm.blobs))
	for i, mh := range tm.blobs {
		owners[i] = pl.Owners(mh)
	}
	tm.mu.Lock()
	tm.plKey, tm.owners = key, owners
	tm.mu.Unlock()
	return owners
}

// tree returns target's memoized structure walk, walking it when the memo is missing or
// a removal invalidated it. structMissing > 0: the structure is incomplete (not memoized).
func (m *Manager) tree(target cid.CID, size uint64, profile string) (tm *treeMemo, structMissing int, err error) {
	gen := m.cfg.Store.Removals()
	key := memoKey{target, profile}
	if tm = m.memo.get(key, gen); tm != nil {
		return tm, 0, nil
	}
	if tm, structMissing, err = m.walk(target, size, profile, gen); err != nil {
		return nil, 0, err
	}
	if structMissing == 0 {
		m.memo.put(key, tm)
	}
	return tm, structMissing, nil
}

type memoKey struct {
	root    cid.CID
	profile string
}

type memoCache struct {
	mu      sync.Mutex
	m       map[memoKey]*treeMemo
	order   []memoKey // insertion order, for eviction
	entries int
}

func (c *memoCache) get(k memoKey, removals uint64) *treeMemo {
	c.mu.Lock()
	defer c.mu.Unlock()
	tm := c.m[k]
	if tm == nil || tm.removals != removals {
		return nil
	}
	return tm
}

func (c *memoCache) put(k memoKey, tm *treeMemo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[memoKey]*treeMemo{}
	}
	if old := c.m[k]; old != nil {
		c.entries -= len(old.blobs)
	} else {
		c.order = append(c.order, k)
	}
	c.m[k] = tm
	c.entries += len(tm.blobs)
	for c.entries > memoMaxBlobs && len(c.order) > 1 {
		ev := c.order[0]
		c.order = c.order[1:]
		if old := c.m[ev]; old != nil {
			c.entries -= len(old.blobs)
			delete(c.m, ev)
		}
	}
}

func linksFor(profile string) func(cid.CID, []byte) ([]fetch.Link, error) {
	if strings.HasPrefix(profile, "atrepo") {
		return func(c cid.CID, data []byte) ([]fetch.Link, error) {
			return fetch.AtrepoLinks(c, data, profile == "atrepo+blobs")
		}
	}
	return fetch.Links
}

// walk reads target's structure from the store under profile, a level at a time. It
// returns the blobs with the held state found, and the number of structure nodes not held
// (whose subtrees are then unknown).
func (m *Manager) walk(target cid.CID, size uint64, profile string, removals uint64) (*treeMemo, int, error) {
	links := linksFor(profile)
	st := m.cfg.Store
	tm := &treeMemo{removals: removals, witness: -1}
	// Keyed by full CID, and every answer is per CID (store.HasMany/GetMany apply Has's
	// codec rules): bytes held only as raw are not held DRISL structure (0077), even when
	// the same tree also links them as a raw blob.
	seen := map[cid.CID]bool{}
	structMissing := 0
	frontier := []fetch.Link{{CID: target, Size: size}}
	for len(frontier) > 0 {
		var level []fetch.Link
		var cs []cid.CID
		for _, l := range frontier {
			if seen[l.CID] {
				continue
			}
			seen[l.CID] = true
			level = append(level, l)
			cs = append(cs, l.CID)
		}
		has, err := st.HasMany(cs)
		if err != nil {
			return nil, 0, err
		}
		var structs []cid.CID
		for _, l := range level {
			info, ok := has[l.CID]
			held := ok && info.Complete
			if l.CID.Codec() == cid.Raw {
				sz := l.Size
				if held {
					sz = info.Size
				}
				tm.blobs = append(tm.blobs, l.CID.Multihash())
				tm.sizes = append(tm.sizes, sz)
				// A --nocopy source can change without a store removal: never memoized held.
				tm.held = append(tm.held, held && !info.NoCopy)
				continue
			}
			if !held {
				structMissing++
				continue
			}
			structs = append(structs, l.CID)
		}
		blocks, err := st.GetMany(structs)
		if err != nil {
			return nil, 0, err
		}
		frontier = nil
		for _, c := range structs {
			data, ok := blocks[c]
			if !ok { // removed since the lookup
				structMissing++
				continue
			}
			kids, err := links(c, data)
			if err != nil {
				return nil, 0, err
			}
			frontier = append(frontier, kids...)
		}
	}
	return tm, structMissing, nil
}

// check reports target's completeness under a placement: every structure node must be
// held, and every blob this member owns (all blobs when full). It also lists the blobs and,
// for full checks, the held ones with their sizes. Atrepo records are leaves except for
// selected in-limit blob references.
func (m *Manager) check(target cid.CID, size uint64, pl *cluster.Placement, full bool, profile string) checkResult {
	res := checkResult{allHeld: true, held: map[cid.Multihash]int64{}}
	st := m.cfg.Store
	tm, missing, err := m.tree(target, size, profile)
	if err != nil {
		res.err = err
		return res
	}
	res.structMissing = missing
	owners := tm.ownersFor(pl)
	tm.mu.Lock()
	held := append([]bool(nil), tm.held...)
	sizes := append([]uint64(nil), tm.sizes...)
	witness := tm.witness
	tm.mu.Unlock()
	owns := make([]bool, len(tm.blobs))
	for i := range tm.blobs {
		owns[i] = full || containsPeer(owners[i], m.cfg.Self)
	}
	// Re-query the blobs not known held: every owned one; for the others, only whether
	// one is missing (allHeld): the last missing one first, then batches until one is.
	var want, rest []int
	for i := range tm.blobs {
		switch {
		case held[i]:
		case owns[i]:
			want = append(want, i)
		case i != witness:
			rest = append(rest, i)
		}
	}
	if witness >= 0 && !owns[witness] && !held[witness] {
		rest = append([]int{witness}, rest...)
	}
	query := func(idx []int) (missing int, err error) {
		missing = -1
		cs := make([]cid.CID, len(idx))
		for j, i := range idx {
			cs[j] = cid.New(cid.Raw, tm.blobs[i]) // blobs are raw CIDs (walk)
		}
		has, err := st.HasMany(cs)
		if err != nil {
			return -1, err
		}
		tm.mu.Lock()
		defer tm.mu.Unlock()
		for j, i := range idx {
			info, ok := has[cs[j]]
			if !ok || !info.Complete {
				if missing < 0 {
					missing = i
				}
				continue
			}
			held[i], sizes[i] = true, info.Size
			if !info.NoCopy {
				tm.held[i], tm.sizes[i] = true, info.Size
			}
		}
		if missing >= 0 && !owns[missing] {
			tm.witness = missing
		}
		return missing, nil
	}
	if len(want) > 0 {
		if _, err := query(want); err != nil {
			res.err = err
			return res
		}
	}
	for len(rest) > 0 {
		n := min(256, len(rest))
		if rest[0] == witness {
			n = 1 // the witness alone first
		}
		miss, err := query(rest[:n])
		if err != nil {
			res.err = err
			return res
		}
		rest = rest[n:]
		if miss >= 0 {
			break
		}
	}
	res.blobs = tm.blobs
	for i, mh := range tm.blobs {
		sz := sizes[i]
		res.allHeld = res.allHeld && held[i]
		if held[i] {
			res.held[mh] = int64(sz)
		}
		if !owns[i] {
			continue
		}
		res.owned.Blobs++
		res.owned.Bytes += int64(sz)
		if !held[i] {
			res.missing.Blobs++
			res.missing.Bytes += int64(sz)
		}
	}
	if len(rest) > 0 { // not queried: some blob is already known missing
		res.allHeld = false
	}
	return res
}

func containsPeer(ps []wire.PeerID, id wire.PeerID) bool {
	for _, p := range ps {
		if p == id {
			return true
		}
	}
	return false
}
