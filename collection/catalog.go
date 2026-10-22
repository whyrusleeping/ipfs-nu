package collection

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
)

// maxDeltaFetch bounds the objects one level of an incremental catalog sync fetches one
// at a time; a larger change (a joining member, a large backfill) fetches the whole
// catalog in one walk instead.
const maxDeltaFetch = 512

// syncWriterCatalog makes the catalog of a writer head (tree header target) complete in
// the store. known are headers of catalogs this node holds complete and pinned (the
// writer's previous head, the merged catalog, other writers' synced heads): every object
// reachable from them is complete with everything below it, so only the part of target
// that differs from them is checked, and only its missing objects are fetched. Without
// known catalogs, or for a change too large for that, the whole catalog is walked (and
// fetched) as one.
func (m *Manager) syncWriterCatalog(ctx context.Context, target cid.CID, known []cid.CID) error {
	if len(known) > 0 {
		done, err := m.deltaCatalog(ctx, target, known)
		if err != nil || done {
			return err
		}
	}
	pin := catalogPin(target, "")
	if m.heldCatalog(ctx, pin) {
		return nil
	}
	if m.cfg.FetchCatalog == nil {
		return errors.New("catalog not held and no network")
	}
	fctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	err := m.cfg.FetchCatalog(fctx, target, nil)
	cancel()
	if err == nil && !m.heldCatalog(ctx, pin) {
		err = errors.New("catalog incomplete after fetch")
	}
	return err
}

// catalogKids splits a catalog object's nu-catalog links into structure (the header's
// root, MST subtrees and container directories, which have links of their own) and
// leaves (metadata records).
func catalogKids(c cid.CID, data []byte) (structure, leaves []cid.CID, err error) {
	if c.Codec() != cid.DRISL {
		return nil, nil, nil
	}
	v, err := drisl.Decode(data)
	if err != nil {
		return nil, nil, err
	}
	mv, ok := v.(map[string]any)
	if !ok {
		return nil, nil, nil
	}
	if t := mv["$type"]; t != nil {
		if t == tree.TypeHeader {
			structure, err = tree.Links(c, data)
		}
		return structure, nil, err
	}
	if _, ok := mv["e"]; !ok {
		return nil, nil, nil
	}
	n, err := tree.DirProfile.Codec.Decode(data)
	if err != nil {
		return nil, nil, err
	}
	if n.Left.Defined() {
		structure = append(structure, n.Left)
	}
	for _, e := range n.Entries {
		switch {
		case e.Val.Meta.Defined():
			leaves = append(leaves, e.Val.Meta)
		case e.Val.Kind == tree.KindDir:
			structure = append(structure, e.Val.CID)
		}
		if e.Right.Defined() {
			structure = append(structure, e.Right)
		}
	}
	return structure, leaves, nil
}

// deltaCatalog checks target level by level against the known complete catalogs, which
// it expands level by level alongside: an object also reachable from them is complete and is
// neither checked nor descended into, and an object below a known object that target
// shares is never visited, so the cost follows the size of the change rather than of the
// catalog. Missing objects are fetched one at a time (their children are checked on the
// next level). It reports false, having changed nothing that matters, when a level
// needs more than maxDeltaFetch fetches.
func (m *Manager) deltaCatalog(ctx context.Context, target cid.CID, known []cid.CID) (bool, error) {
	st := m.cfg.Store
	// Keyed by full CID: bytes known as a leaf do not make the same bytes linked as a
	// node complete (0079).
	oldSeen := map[cid.CID]bool{}
	newSeen := map[cid.CID]bool{target: true}
	var oldFront []cid.CID
	for _, k := range known {
		if !oldSeen[k] {
			oldSeen[k] = true
			oldFront = append(oldFront, k)
		}
	}
	// shared are known objects target also has; their subtrees need no look, and parent
	// links let a level of known objects already expanded below one be skipped (when the
	// MST above it grew by a level).
	shared := map[cid.CID]bool{}
	parent := map[cid.CID]cid.CID{}
	expandOld := func() error {
		var next []cid.CID
		for _, o := range oldFront {
			h := o
			if p, ok := parent[h]; newSeen[h] || shared[h] || (ok && shared[p]) {
				shared[h] = true
				continue
			}
			data, err := st.Get(o)
			if err != nil {
				return fmt.Errorf("known catalog object %s: %w", o, err)
			}
			structure, leaves, err := catalogKids(o, data)
			if err != nil {
				return err
			}
			for _, l := range leaves {
				oldSeen[l] = true
			}
			for _, s := range structure {
				if !oldSeen[s] {
					oldSeen[s] = true
					parent[s] = h
					next = append(next, s)
				}
			}
		}
		oldFront = next
		return nil
	}
	// Each level of target is checked after the known catalogs' same level is expanded,
	// so an object one level deeper there (the MST above it shrank) still matches.
	front := []cid.CID{target}
	for len(front) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := expandOld(); err != nil {
			return false, err
		}
		var check, missing []cid.CID
		for _, c := range front {
			if oldSeen[c] {
				shared[c] = true
				continue
			}
			info, ok, err := st.Has(c)
			if err != nil {
				return false, err
			}
			if !ok || !info.Complete {
				missing = append(missing, c)
			}
			check = append(check, c)
		}
		if len(missing) > 0 {
			if m.cfg.FetchCatalog == nil {
				return false, errors.New("catalog not held and no network")
			}
			if len(missing) > maxDeltaFetch {
				return false, nil
			}
			if err := m.fetchEach(ctx, missing); err != nil {
				return false, err
			}
		}
		var next []cid.CID
		for _, c := range check {
			if c.Codec() != cid.DRISL {
				continue
			}
			data, err := st.Get(c)
			if err != nil {
				return false, fmt.Errorf("catalog object %s: %w", c, err)
			}
			structure, leaves, err := catalogKids(c, data)
			if err != nil {
				return false, fmt.Errorf("catalog object %s: %w", c, err)
			}
			for _, k := range append(structure, leaves...) {
				if !newSeen[k] {
					newSeen[k] = true
					next = append(next, k)
				}
			}
		}
		front = next
	}
	return true, nil
}

// fetchEach fetches catalog objects without their children, 16 at a time.
func (m *Manager) fetchEach(ctx context.Context, cs []cid.CID) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	skipAll := func(cid.CID) bool { return true }
	sem := make(chan struct{}, 16)
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for _, c := range cs {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := m.cfg.FetchCatalog(ctx, c, skipAll); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}
