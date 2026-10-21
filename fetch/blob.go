package fetch

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/proto"
	"github.com/whyrusleeping/nu/store"
)

// blob fetches a raw blob. size is authenticated (from a parent) or 0 for unknown.
func (r *run) blob(ctx context.Context, c cid.CID, size uint64) error {
	info, ok, err := r.st.Has(c)
	if err != nil {
		return err
	}
	if ok && info.Complete {
		r.update(func(p *Progress) { p.Bytes += int64(info.Size) })
		return nil
	}
	if c.Hash() != cid.BLAKE3 || (size > 0 && !blob.HasOutboard(size)) {
		if err := r.whole(ctx, c); err != nil {
			return err
		}
		if info, ok, err := r.st.Has(c); err == nil && ok {
			r.update(func(p *Progress) {
				p.Bytes += int64(info.Size)
				if size == 0 {
					p.BytesTotal += int64(info.Size)
				}
			})
		}
		return nil
	}
	// One download per blob per Fetcher; later callers wait for it and re-check the store.
	f := r.f
	for {
		f.mu.Lock()
		in := f.blobs[c.Multihash()]
		if in == nil {
			in = &inflight{done: make(chan struct{})}
			f.blobs[c.Multihash()] = in
			f.mu.Unlock()
			in.err = r.largeBlob(ctx, c, size)
			f.mu.Lock()
			delete(f.blobs, c.Multihash())
			f.mu.Unlock()
			close(in.done)
			return in.err
		}
		f.mu.Unlock()
		select {
		case <-in.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if info, ok, err := r.st.Has(c); err != nil || (ok && info.Complete) {
			return err
		}
	}
}

// holding is what a peer's /nu/have/1 answer says it holds of a blob.
type holding struct {
	size     uint64
	complete bool
	ranges   []blob.Range
}

func (h holding) covers(g blob.Range) bool {
	if h.complete {
		return true
	}
	for _, r := range h.ranges {
		if r.Start <= g.Start && g.End <= r.End {
			return true
		}
	}
	return false
}

// have asks every live peer about mh in parallel. With a known size, partial holders are
// asked about every window (up to MaxHaveItems windows).
func (r *run) have(ctx context.Context, mh cid.Multihash, size uint64) map[*peer]holding {
	var qs []proto.HaveQuery
	if size == 0 {
		qs = []proto.HaveQuery{{MH: mh}}
	} else {
		groups := blob.Groups(size)
		for s := uint64(0); s < groups && len(qs) < proto.MaxHaveItems; s += proto.MaxHaveWindow {
			qs = append(qs, proto.HaveQuery{MH: mh, Window: &blob.Range{Start: s, End: min(s+proto.MaxHaveWindow, groups)}})
		}
	}
	var mu sync.Mutex
	out := map[*peer]holding{}
	var wg sync.WaitGroup
	for _, p := range r.sw.live() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rctx, cancel := r.timeout(ctx)
			res, err := proto.Have(rctx, p.Open, qs)
			cancel()
			if err != nil {
				r.sw.report(ctx, p, err)
				return
			}
			var h holding
			for i, a := range res {
				if !a.Held || (i > 0 && a.Size != h.size) {
					continue
				}
				h.size, h.complete = a.Size, h.complete || a.Complete
				h.ranges = append(h.ranges, a.Ranges...)
			}
			if h.size > 0 || res[0].Held {
				mu.Lock()
				out[p] = h
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}

// largeBlob downloads a BLAKE3 blob across peers into a partial blob and commits it.
func (r *run) largeBlob(ctx context.Context, c cid.CID, size uint64) error {
	mh := c.Multihash()
	psize, held, _, err := r.st.HeldRanges(mh)
	if err != nil {
		return err
	}
	groupsHeld := func(s uint64) bool { // the last group is held: size proven by Bao
		g := blob.Groups(s)
		return len(held) > 0 && held[len(held)-1].End == g
	}
	unknown := size == 0
	if size > 0 && psize > 0 && psize != size {
		if err := r.st.DiscardPartial(mh); err != nil {
			return err
		}
		psize = 0
	}
	if size == 0 && psize > 0 && groupsHeld(psize) {
		size = psize
	}
	holders := r.have(ctx, mh, size)
	proven := size > 0
	for !proven {
		// Choose a claimed size, preferring the partial's, and prove it by fetching the
		// last group from a peer that claimed it.
		votes := map[uint64]int{}
		for _, h := range holders {
			votes[h.size]++
		}
		if len(votes) == 0 {
			return fmt.Errorf("%w: %s", ErrNoProvider, c)
		}
		cand := psize
		if votes[cand] == 0 {
			cand = slices.MaxFunc(slices.Collect(maps.Keys(votes)), func(a, b uint64) int { return votes[a] - votes[b] })
		}
		if !blob.HasOutboard(cand) {
			if err := r.whole(ctx, c); err != nil {
				return err
			}
			r.update(func(p *Progress) { p.Bytes += int64(cand); p.BytesTotal += int64(cand) })
			return nil
		}
		if psize > 0 && psize != cand {
			if err := r.st.DiscardPartial(mh); err != nil {
				return err
			}
			psize = 0
		}
		last := blob.Range{Start: blob.Groups(cand) - 1, End: blob.Groups(cand)}
		var prover *peer
		for p, h := range holders {
			if h.size == cand && h.covers(last) {
				prover = p
				break
			}
		}
		if prover == nil {
			for p, h := range holders {
				if h.size == cand {
					delete(holders, p)
				}
			}
			continue
		}
		part, err := r.st.Partial(mh, cand)
		if err != nil {
			return err
		}
		rctx, cancel := r.timeout(ctx)
		err = proto.FetchRanges(rctx, prover.Open, part, []blob.Range{last})
		cancel()
		if cerr := part.Close(); err == nil {
			err = cerr
		}
		switch {
		case err == nil:
			size, proven = cand, true
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, proto.ErrCorrupt):
			r.sw.drop(prover, err)
			delete(holders, prover)
			if err := r.st.DiscardPartial(mh); err != nil {
				return err
			}
			psize = 0
		default:
			r.sw.report(ctx, prover, err)
			delete(holders, prover)
			psize = cand
		}
	}
	if psize == 0 && size == 0 {
		return fmt.Errorf("%w: %s", ErrNoProvider, c)
	}
	// Holders that claimed another size are provably wrong.
	for p, h := range holders {
		if h.size != size {
			r.sw.drop(p, fmt.Errorf("claimed size %d for a %d-byte blob", h.size, size))
			delete(holders, p)
		}
	}
	if unknown {
		r.update(func(p *Progress) { p.BytesTotal += int64(size) })
	}
	part, err := r.st.Partial(mh, size)
	if errors.Is(err, store.ErrComplete) {
		return nil
	} else if err != nil {
		return err
	}
	err = r.download(ctx, part, holders)
	if err != nil {
		return errors.Join(err, part.Close())
	}
	if err := part.Commit(); err != nil {
		return errors.Join(err, part.Close())
	}
	return nil
}

// chunk is a fixed range of groups scheduled as one request.
type chunk struct {
	r        blob.Range
	bytes    int64 // missing bytes when the download started
	done     bool
	fetchers map[*peer]context.CancelFunc
	refused  map[*peer]bool // peers that answered "not held"
}

// sched hands chunks to per-peer workers: first unclaimed chunks, then, in the endgame,
// a second request for chunks still in flight. The first copy to arrive wins; the other is
// cancelled.
type sched struct {
	mu        sync.Mutex
	cond      *sync.Cond
	chunks    []*chunk
	first     int // index before which every chunk is done
	remaining int
	busy      int
	holders   map[*peer]holding
	stuck     bool
}

func (s *sched) canServe(p *peer, c *chunk) bool {
	return !c.done && !c.refused[p] && s.holders[p].covers(c.r)
}

func (s *sched) pick(p *peer) *chunk {
	for s.first < len(s.chunks) && s.chunks[s.first].done {
		s.first++
	}
	var dup *chunk
	for _, c := range s.chunks[s.first:] {
		if !s.canServe(p, c) || c.fetchers[p] != nil {
			continue
		}
		if len(c.fetchers) == 0 {
			return c
		}
		if dup == nil && len(c.fetchers) < 2 {
			dup = c
		}
	}
	return dup
}

// download fetches the missing groups of part from holders.
func (r *run) download(ctx context.Context, part *store.Partial, holders map[*peer]holding) error {
	cg := uint64(r.f.cfg.ChunkGroups)
	groups := blob.Groups(part.Size)
	s := &sched{holders: holders}
	s.cond = sync.NewCond(&s.mu)
	var heldBytes int64
	for start := uint64(0); start < groups; start += cg {
		c := &chunk{r: blob.Range{Start: start, End: min(start+cg, groups)}, fetchers: map[*peer]context.CancelFunc{}, refused: map[*peer]bool{}}
		missing := missingIn(part, c.r)
		for _, m := range missing {
			_, n := m.Bytes(part.Size)
			c.bytes += int64(n)
		}
		_, n := c.r.Bytes(part.Size)
		heldBytes += int64(n) - c.bytes
		if len(missing) > 0 {
			s.chunks = append(s.chunks, c)
		}
	}
	s.remaining = len(s.chunks)
	r.update(func(p *Progress) { p.Bytes += heldBytes })
	stop := context.AfterFunc(ctx, func() { s.mu.Lock(); s.cond.Broadcast(); s.mu.Unlock() })
	defer stop()

	var flushMu sync.Mutex
	var sinceFlush uint64
	flush := func(n int64) error {
		flushMu.Lock()
		defer flushMu.Unlock()
		if sinceFlush += uint64(n); sinceFlush < r.f.cfg.FlushBytes {
			return nil
		}
		sinceFlush = 0
		return part.Flush()
	}

	worker := func(p *peer) {
		for {
			s.mu.Lock()
			var c *chunk
			for {
				if s.remaining == 0 || s.stuck || ctx.Err() != nil || !r.sw.usable(p) {
					s.cond.Broadcast()
					s.mu.Unlock()
					return
				}
				if c = s.pick(p); c != nil {
					break
				}
				if s.busy == 0 && !s.anyCandidate(r.sw) {
					s.stuck = true
					continue
				}
				s.cond.Wait()
			}
			cctx, cancel := r.timeout(ctx)
			c.fetchers[p] = cancel
			s.busy++
			s.mu.Unlock()

			var err error
			if need := missingIn(part, c.r); len(need) > 0 {
				err = proto.FetchRanges(cctx, p.Open, part, need)
			}
			lost := cctx.Err() != nil && ctx.Err() == nil // cancelled because another copy won
			cancel()

			s.mu.Lock()
			delete(c.fetchers, p)
			s.busy--
			won := false
			switch {
			case err == nil && !c.done:
				c.done, won = true, true
				s.remaining--
				for _, other := range c.fetchers {
					other()
				}
			case err != nil && lost && c.done:
			case errors.Is(err, proto.ErrNotHeld):
				c.refused[p] = true
			case err != nil:
				r.sw.report(ctx, p, err)
			}
			s.cond.Broadcast()
			s.mu.Unlock()
			if won {
				r.update(func(pr *Progress) { pr.Bytes += c.bytes })
				if err := flush(c.bytes); err != nil {
					s.mu.Lock()
					s.stuck = true
					s.cond.Broadcast()
					s.mu.Unlock()
				}
			}
		}
	}
	var wg sync.WaitGroup
	for p := range holders {
		for range r.f.cfg.PeerRequests {
			wg.Add(1)
			go func() { defer wg.Done(); worker(p) }()
		}
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.remaining > 0 || !part.Complete() {
		return fmt.Errorf("%w: %s: %d of %d ranges unavailable", ErrNoProvider, part.MH, s.remaining, len(s.chunks))
	}
	return nil
}

// anyCandidate reports whether some usable peer could still serve some unfinished chunk.
func (s *sched) anyCandidate(sw *swarm) bool {
	for p := range s.holders {
		if !sw.usable(p) {
			continue
		}
		for _, c := range s.chunks[s.first:] {
			if s.canServe(p, c) {
				return true
			}
		}
	}
	return false
}

// missingIn returns the groups of r that part does not hold, as ranges.
func missingIn(part *store.Partial, r blob.Range) []blob.Range {
	var out []blob.Range
	for g := r.Start; g < r.End; g++ {
		if part.Has(blob.Range{Start: g, End: g + 1}) {
			continue
		}
		if l := len(out); l > 0 && out[l-1].End == g {
			out[l-1].End++
		} else {
			out = append(out, blob.Range{Start: g, End: g + 1})
		}
	}
	return out
}
