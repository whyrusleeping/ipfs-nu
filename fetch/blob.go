package fetch

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

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
	if c.Hash() != cid.BLAKE3 || (size > 0 && size <= r.oneRequest()) {
		// Held was checked just above; the store's one connection is the bottleneck of
		// many-small-file fetches, so no lookup is repeated that the walk can answer.
		if err := r.fetchWhole(ctx, c, size); err != nil {
			return err
		}
		if size == 0 {
			if info, ok, err := r.st.Has(c); err == nil && ok {
				size = info.Size
				r.update(func(p *Progress) { p.BytesTotal += int64(size) })
			}
		}
		r.update(func(p *Progress) { p.Bytes += int64(size) })
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
			in.err = r.largeBlobAny(ctx, c, size)
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

// largeBlobAny fetches a large BLAKE3 blob by ranges from peers, or whole from the HTTPS
// source when there are no usable peers or they all failed (a RASL host serves any size;
// verification is the whole-blob Bao check in the store).
func (r *run) largeBlobAny(ctx context.Context, c cid.CID, size uint64) error {
	var err error
	if len(r.sw.live()) > 0 {
		err = r.largeBlob(ctx, c, size)
		if err == nil || r.opts.Source == nil || ctx.Err() != nil {
			return err
		}
	}
	if r.opts.Source == nil {
		return fmt.Errorf("%w: %s", ErrNoProvider, c)
	}
	if serr := r.opts.Source.FetchObject(ctx, c, size); serr != nil {
		return errors.Join(err, serr)
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
			select {
			case p.haves <- struct{}{}:
			case <-ctx.Done():
				return
			}
			rctx, cancel := r.timeout(ctx)
			res, err := proto.Have(rctx, p.Open, qs)
			cancel()
			<-p.haves
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
	opened := r.lanes().beginOpen()
	defer opened()
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
		if err := prover.acquire(ctx); err != nil {
			return errors.Join(err, part.Close())
		}
		rctx, cancel := r.timeout(ctx)
		err = proto.FetchRanges(rctx, prover.Open, part, []blob.Range{last})
		cancel()
		prover.release()
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
	err = r.download(ctx, part, holders, opened)
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

// sched is one large blob's download: its chunks, holders and checkpoint state. The
// scheduling fields are guarded by the run's lanes.mu.
type sched struct {
	ctx       context.Context
	part      *store.Partial
	chunks    []*chunk
	first     int // index before which every chunk is done
	remaining int
	busy      int // claimed chunks whose request (and the winner's checkpoint) is running
	holders   map[*peer]holding
	finished  bool
	done      chan struct{} // closed when finished
	err       error         // local checkpoint failure, not a provider failure

	flushMu    sync.Mutex
	sinceFlush uint64
	flushing   chan struct{}
	lastFlush  time.Time // when the last checkpoint began
}

func (s *sched) canServe(p *peer, c *chunk) bool {
	return !c.done && !c.refused[p] && s.holders[p].covers(c.r)
}

// open returns the chunks from the first one not done.
func (s *sched) open() []*chunk {
	for s.first < len(s.chunks) && s.chunks[s.first].done {
		s.first++
	}
	return s.chunks[s.first:]
}

func (s *sched) finish() {
	if !s.finished {
		s.finished = true
		close(s.done)
	}
}

// anyCandidate reports whether some usable peer could still serve some unfinished chunk.
func (s *sched) anyCandidate(sw *swarm) bool {
	for p := range s.holders {
		if !sw.usable(p) {
			continue
		}
		for _, c := range s.open() {
			if s.canServe(p, c) {
				return true
			}
		}
	}
	return false
}

// flush counts n verified bytes and checkpoints the partial once `every` bytes have
// arrived and `interval` has passed since the last checkpoint began. Only one worker
// checkpoints at a time; the others keep downloading instead of queueing behind fsync,
// until a checkpoint has run for longer than interval: then the disk is stalled and they
// wait, so uncheckpointed data stays bounded (about two intervals of transfer). Flush
// snapshots the bitmap before syncing; groups verified meanwhile stay dirty for the next.
func (s *sched) flush(every uint64, interval time.Duration, n int64) error {
	s.flushMu.Lock()
	s.sinceFlush += uint64(n)
	for s.sinceFlush >= every {
		if s.flushing != nil {
			if time.Since(s.lastFlush) < interval {
				break
			}
			done := s.flushing
			s.flushMu.Unlock()
			<-done
			s.flushMu.Lock()
			continue
		}
		if time.Since(s.lastFlush) < interval {
			break
		}
		s.sinceFlush, s.flushing, s.lastFlush = 0, make(chan struct{}), time.Now()
		s.flushMu.Unlock()
		err := s.part.Flush()
		s.flushMu.Lock()
		close(s.flushing)
		s.flushing = nil
		s.flushMu.Unlock()
		return err
	}
	s.flushMu.Unlock()
	return nil
}

// lanes schedules the range requests of every large blob one run downloads. Each holder
// peer has workers that take the first unclaimed chunk they can serve from any open
// download, in start order, so blobs of a few chunks keep every peer busy too. A chunk
// already in flight is requested from a second peer (the endgame: the first copy wins,
// the other is cancelled) only when no chunk of the run is unclaimed and no blob is still
// being opened, that is at the tail of the run rather than of every blob.
type lanes struct {
	r       *run
	mu      sync.Mutex
	cond    *sync.Cond
	jobs    []*sched
	workers map[*peer]int
	opening int
	closed  bool
	wg      sync.WaitGroup
}

func (r *run) lanes() *lanes {
	r.lanesOnce.Do(func() {
		r.ln = &lanes{r: r, workers: map[*peer]int{}}
		r.ln.cond = sync.NewCond(&r.ln.mu)
	})
	return r.ln
}

// close stops the workers once no download is open (the end of the Fetch).
func (l *lanes) close() {
	l.mu.Lock()
	l.closed = true
	l.cond.Broadcast()
	l.mu.Unlock()
	l.wg.Wait()
}

// beginOpen counts a blob being prepared (have, size proof) until it registers or gives
// up, so the endgame waits for it. The returned func ends it and may be called again.
func (l *lanes) beginOpen() func() {
	l.mu.Lock()
	l.opening++
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.opening--
			l.cond.Broadcast()
			l.mu.Unlock()
		})
	}
}

// start gives each usable holder PeerRequests workers per open download, up to
// PeerRequests×Blobs and the peer's request slots. Called with mu held.
func (l *lanes) start(holders map[*peer]holding) {
	cfg := l.r.f.cfg
	for p := range holders {
		if !l.r.sw.usable(p) {
			continue
		}
		target := min(cfg.PeerRequests*len(l.jobs), cfg.PeerRequests*cfg.Blobs, cap(p.slots))
		for ; l.workers[p] < target; l.workers[p]++ {
			l.wg.Add(1)
			go l.worker(p)
		}
	}
}

// pick returns the next chunk for p: the first unclaimed one it can serve, or in the
// endgame a chunk one other peer is fetching. Called with mu held.
func (l *lanes) pick(p *peer) (*sched, *chunk) {
	unclaimed := false
	for _, s := range l.jobs {
		if s.finished {
			continue
		}
		for _, c := range s.open() {
			if c.done || len(c.fetchers) > 0 {
				continue
			}
			if s.canServe(p, c) {
				return s, c
			}
			unclaimed = true
		}
	}
	if unclaimed || l.opening > 0 {
		return nil, nil
	}
	for _, s := range l.jobs {
		if s.finished {
			continue
		}
		for _, c := range s.open() {
			if len(c.fetchers) == 1 && c.fetchers[p] == nil && s.canServe(p, c) {
				return s, c
			}
		}
	}
	return nil, nil
}

// checkStuck finishes the downloads no usable peer can make progress on. Called with mu
// held.
func (l *lanes) checkStuck() {
	for _, s := range l.jobs {
		if !s.finished && s.busy == 0 && !s.anyCandidate(l.r.sw) {
			s.finish()
		}
	}
}

func (l *lanes) worker(p *peer) {
	defer l.wg.Done()
	r := l.r
	l.mu.Lock()
	defer l.mu.Unlock()
	for {
		if l.closed || !r.sw.usable(p) {
			l.workers[p]--
			l.checkStuck()
			l.cond.Broadcast()
			return
		}
		s, c := l.pick(p)
		if c == nil {
			l.checkStuck()
			l.cond.Wait()
			continue
		}
		cctx, cancel := r.timeout(s.ctx)
		c.fetchers[p] = cancel
		s.busy++
		l.mu.Unlock()

		var err error
		if need := missingIn(s.part, c.r); len(need) > 0 {
			if err = p.acquire(cctx); err == nil {
				err = proto.FetchRanges(cctx, p.Open, s.part, need)
				p.release()
			}
		}
		// Cancelled here: another copy won, or the download is being torn down (otherwise
		// a request timeout, which counts against the peer).
		lost := cctx.Err() != nil && s.ctx.Err() == nil
		cancel()

		l.mu.Lock()
		delete(c.fetchers, p)
		won := false
		switch {
		case err == nil && !c.done:
			c.done, won = true, true
			s.remaining--
			for _, other := range c.fetchers {
				other()
			}
		case err != nil && lost && (c.done || s.finished):
		case errors.Is(err, proto.ErrNotHeld), errors.Is(err, proto.ErrNotFound):
			c.refused[p] = true
		case err != nil:
			r.sw.report(s.ctx, p, err)
		}
		if !won {
			s.busy--
		}
		if err != nil {
			l.checkStuck()
		}
		l.cond.Broadcast()
		busy := errors.Is(err, proto.ErrBusy)
		if !won && !busy {
			continue
		}
		l.mu.Unlock()
		var ferr error
		if won {
			// The winner stays busy until its checkpoint is done, so the download does
			// not commit or close the partial under it.
			r.update(func(pr *Progress) { pr.Bytes += c.bytes })
			ferr = s.flush(r.f.cfg.FlushBytes, r.f.cfg.FlushInterval, c.bytes)
		} else {
			// The peer is loaded by other clients; give it a moment before retrying.
			select {
			case <-time.After(20 * time.Millisecond):
			case <-s.ctx.Done():
			}
		}
		l.mu.Lock()
		if won {
			s.busy--
			if ferr != nil && s.err == nil {
				s.err = ferr
				s.finish()
			}
			if s.remaining == 0 {
				s.finish()
			}
			l.cond.Broadcast()
		}
	}
}

// download fetches the missing groups of part from holders through the run's lanes.
// opened is called once the download is registered (see beginOpen).
func (r *run) download(ctx context.Context, part *store.Partial, holders map[*peer]holding, opened func()) error {
	cg := uint64(r.f.cfg.ChunkGroups)
	groups := blob.Groups(part.Size)
	s := &sched{ctx: ctx, part: part, holders: holders, done: make(chan struct{})}
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

	l := r.lanes()
	l.mu.Lock()
	if s.remaining == 0 {
		s.finish()
	} else {
		l.jobs = append(l.jobs, s)
		l.start(holders)
		l.checkStuck()
		l.cond.Broadcast()
	}
	l.mu.Unlock()
	opened()
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	l.mu.Lock()
	s.finish()
	l.jobs = slices.DeleteFunc(l.jobs, func(j *sched) bool { return j == s })
	// Requests still running (lost endgame copies, or all of them on cancellation or a
	// checkpoint failure) write into part: let them unwind before it is committed or
	// closed.
	for s.busy > 0 {
		for _, c := range s.chunks {
			if !c.done || ctx.Err() != nil || s.err != nil {
				for _, cancel := range c.fetchers {
					cancel()
				}
			}
		}
		l.cond.Wait()
	}
	l.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.err != nil {
		return s.err
	}
	if s.remaining > 0 || !part.Complete() {
		return fmt.Errorf("%w: %s: %d of %d ranges unavailable", ErrNoProvider, part.MH, s.remaining, len(s.chunks))
	}
	return nil
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
