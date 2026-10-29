package fetch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/proto"
	"github.com/whyrusleeping/nu/store"
)

// ErrNoProvider means no live candidate peer supplied the content.
var ErrNoProvider = errors.New("fetch: no provider has the content")

// Config configures a Fetcher. Zero values select the defaults.
type Config struct {
	Store     *store.Store
	Providers Providers // may be nil when every fetch passes Options.Peers
	// PeerRequests is the number of concurrent range requests per peer for each large blob
	// being downloaded (4), up to PeerRequests×Blobs per peer.
	PeerRequests int
	// ChunkGroups is the size of a blob range request in 16 KiB groups (64, i.e. 1 MiB).
	ChunkGroups int
	// Structure is the number of structure nodes fetched in parallel per level (16).
	Structure int
	// Blobs scales how many blobs are fetched in parallel (4): small ones while their sizes
	// fit Blobs×16 MiB, at most 64×Blobs; large ones while their sizes (each counted as 1
	// to 64 MiB) fit Blobs×64 MiB, at most 16×Blobs.
	Blobs int
	// RequestTimeout bounds one request; a stalled peer is then treated as failing (2 min).
	RequestTimeout time.Duration
	// FlushBytes is how often partial blobs persist their verified groups (16 MiB).
	FlushBytes uint64
}

// Fetcher downloads content by CID from multiple peers into a store, verifying everything
// before it is stored or served. It is safe for concurrent use; concurrent fetches of the
// same blob share one download.
type Fetcher struct {
	cfg   Config
	mu    sync.Mutex
	blobs map[cid.Multihash]*inflight
}

type inflight struct {
	done chan struct{}
	err  error
}

// New returns a fetcher.
func New(cfg Config) *Fetcher {
	if cfg.PeerRequests <= 0 {
		cfg.PeerRequests = 4
	}
	if cfg.ChunkGroups <= 0 || cfg.ChunkGroups > proto.MaxRanges {
		cfg.ChunkGroups = proto.MaxRanges
	}
	if cfg.Structure <= 0 {
		cfg.Structure = 16
	}
	if cfg.Blobs <= 0 {
		cfg.Blobs = 4
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 2 * time.Minute
	}
	if cfg.FlushBytes == 0 {
		cfg.FlushBytes = 16 << 20
	}
	return &Fetcher{cfg: cfg, blobs: map[cid.Multihash]*inflight{}}
}

// Stages reported in Progress.
const (
	StageProviders = "providers"
	StageStructure = "structure"
	StageBlobs     = "blobs"
	StageDone      = "done"
)

// Progress is a snapshot of a fetch. Totals grow while the walk discovers content.
type Progress struct {
	Stage                 string
	Objects, ObjectsTotal int64
	Bytes, BytesTotal     int64 // blob bytes
}

// Options configures one fetch.
type Options struct {
	// Profile selects nu-tree (default), atrepo, or atrepo+blobs.
	Profile string
	// Size is the target's size from an authenticated source (tree entry, file node), for
	// raw blob targets; 0 means unknown.
	Size uint64
	// Peers are extra candidates for this fetch (link hints), tried with the Providers.
	Peers []Peer
	// Have lists complete trees the store already holds (an older version of the target),
	// so peers send only what changed (/nu/tree/1 have diff).
	Have []cid.CID
	// Progress, if set, is called (serialized) whenever progress changes.
	Progress func(Progress)
	// Catalog fetches only a collection catalog (the nu-catalog walk profile: header,
	// container directories and item metadata records, not item contents), layer 1 only.
	Catalog bool
	// Source is an optional HTTPS whole-object source (D15: RASL hints, later FISL
	// locations) asked for each object in parallel with the peers; the first verified
	// copy wins. It is the only source when there are no peers.
	Source Source
	// Want, if set, selects the blobs to fetch (cluster placement: the blobs this member
	// owns). Structure is always fetched; /nu/tree/1 then inlines no blobs.
	Want func(c cid.CID) bool
	// Rank, if set, orders the sources of an object: the peers it returns are tried
	// first, in that order, then the other candidates (D15 placement order).
	Rank func(c cid.CID) []wire.PeerID
	// Skip, if set, names objects below the root the walk leaves out: neither fetched
	// nor descended into (the caller holds them complete, or fetches them itself).
	Skip func(c cid.CID) bool
}

// Source is a non-peer source of whole objects (package rasl implements it over
// httpsrc). FetchObject makes c held and verified in the fetcher's store, or returns an
// error; size is the authenticated size when known, 0 otherwise.
type Source interface {
	FetchObject(ctx context.Context, c cid.CID, size uint64) error
}

// Result summarizes a fetch.
type Result struct {
	Objects int64
	Bytes   int64
	// Dropped lists peers that sent corrupt data during this fetch.
	Dropped []wire.PeerID
}

// GetOptions configures Get.
type GetOptions struct {
	Options
	PinSource string // default store.SourceExplicit
	PinName   string
}

// Get pins c recursively (nu-tree walk profile) and fetches everything it reaches. The pin
// is added first so that fetched objects and partial blobs survive GC and a later Get
// resumes; it remains if the fetch fails. Get returns only when the pinned content is
// complete in the store.
func (f *Fetcher) Get(ctx context.Context, c cid.CID, o GetOptions) (Result, error) {
	pin := store.Pin{CID: c, Mode: store.Recursive, Profile: "nu-tree", Source: o.PinSource, Name: o.PinName}
	if o.Profile != "" {
		pin.Profile = o.Profile
	}
	if pin.Source == "" {
		pin.Source = store.SourceExplicit
	}
	if err := f.cfg.Store.Pin(pin); err != nil {
		return Result{}, err
	}
	res, err := f.Fetch(ctx, c, o.Options)
	if err != nil {
		return res, err
	}
	ws, err := f.cfg.Store.Walk(ctx, pin, nil)
	if err != nil {
		return res, err
	}
	if !ws.Complete() {
		return res, fmt.Errorf("fetch: %d objects still missing under %s", ws.Missing, c)
	}
	return res, nil
}

// Fetch fetches c under Options.Profile (nu-tree by default, or atrepo/atrepo+blobs).
// The nu-tree walk reaches header → directories → entries →
// blobs; MASL file node → blob; or a raw blob alone. Structure and small blobs come first over
// /nu/tree/1 from peers that support it; then a layer-1 walk fetches what is still missing
// level by level over /nu/bytes/1, and the blobs in parallel across peers. Objects already
// held are not fetched again, and partial blobs resume from their verified groups.
func (f *Fetcher) Fetch(ctx context.Context, c cid.CID, o Options) (Result, error) {
	switch o.Profile {
	case "", proto.ProfileNuTree, proto.ProfileAtrepo, "atrepo+blobs":
	default:
		return Result{}, fmt.Errorf("fetch: unsupported profile %q", o.Profile)
	}
	if o.Catalog && o.Profile != "" && o.Profile != proto.ProfileNuTree {
		return Result{}, errors.New("fetch: catalog requires nu-tree")
	}
	r := &run{f: f, st: f.cfg.Store, opts: o, sw: &swarm{index: map[wire.PeerID]*peer{}}}
	r.stage(StageProviders)
	for _, p := range o.Peers {
		r.sw.add(p)
	}
	// Provider discovery streams in the background: the walk starts as soon as any
	// source yields a usable peer (a link hint, a connected peer) and later candidates
	// join the swarm as they arrive, so a slow or failing source (a DHT whose bootstrap
	// is stuck) never delays a transfer the hints can serve. Discovery is cancelled
	// when the walk is done.
	d := r.discover(ctx, c)
	defer d.stop()
	defer func() {
		if r.ln != nil {
			r.ln.close()
		}
	}()
	if len(r.sw.live()) == 0 && o.Source == nil {
		if err := d.waitFirst(ctx); err != nil {
			return r.result(), err
		}
		if err := d.finishedErr(); err != nil && len(r.sw.live()) == 0 {
			return r.result(), err
		}
	}
	gen := r.sw.generation()
	err := r.walk(ctx, c, o.Size)
	if err != nil && ctx.Err() == nil {
		// The peers tried could not serve it; sources still running may name others.
		// Wait for them and retry once if any arrived (held objects are skipped).
		if werr := d.wait(ctx); werr == nil && r.sw.generation() != gen {
			err = r.walk(ctx, c, o.Size)
		}
	}
	if err == nil {
		r.stage(StageDone)
	}
	return r.result(), err
}

// discovery is one Fetch's background Providers.Find.
type discovery struct {
	done   chan struct{} // closed when Find returned
	first  chan struct{} // closed when the first candidate arrived
	once   sync.Once
	err    error
	cancel context.CancelFunc
}

func (r *run) discover(ctx context.Context, c cid.CID) *discovery {
	d := &discovery{done: make(chan struct{}), first: make(chan struct{})}
	ctx, d.cancel = context.WithCancel(ctx)
	if r.f.cfg.Providers == nil {
		close(d.done)
		return d
	}
	go func() {
		defer close(d.done)
		d.err = r.f.cfg.Providers.Find(ctx, c, func(p Peer) {
			r.sw.add(p)
			d.once.Do(func() { close(d.first) })
		})
	}()
	return d
}

// waitFirst returns when a candidate arrived, discovery ended, or ctx ended.
func (d *discovery) waitFirst(ctx context.Context) error {
	select {
	case <-d.first:
	case <-d.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// wait returns when discovery ended or ctx ended.
func (d *discovery) wait(ctx context.Context) error {
	select {
	case <-d.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *discovery) finished() bool {
	select {
	case <-d.done:
		return true
	default:
		return false
	}
}

// finishedErr is Find's error once it returned, nil while it runs.
func (d *discovery) finishedErr() error {
	if d.finished() {
		return d.err
	}
	return nil
}

func (d *discovery) stop() {
	d.cancel()
	<-d.done
}

// run is one Fetch call.
type run struct {
	f    *Fetcher
	st   *store.Store
	opts Options
	sw   *swarm
	next atomic.Uint64 // rotates whole-object requests across peers

	lanesOnce sync.Once
	ln        *lanes // range-request scheduler for large blobs, created on first use
	commits   committer

	pmu  sync.Mutex
	prog Progress
}

func (r *run) result() Result {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	return Result{Objects: r.prog.Objects, Bytes: r.prog.Bytes, Dropped: r.sw.droppedIDs()}
}

func (r *run) update(fn func(*Progress)) {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	fn(&r.prog)
	if r.opts.Progress != nil {
		r.opts.Progress(r.prog)
	}
}

func (r *run) stage(s string) { r.update(func(p *Progress) { p.Stage = s }) }

// swarm is the set of candidate peers of one fetch and what they have done.
type swarm struct {
	mu      sync.Mutex
	peers   []*peer
	index   map[wire.PeerID]*peer
	dropped []wire.PeerID
	gen     int
}

type peer struct {
	Peer
	bad                bool // dropped (corrupt) or failing; guarded by swarm.mu
	fails              int
	connectionFailures map[*host.Conn]bool // bounded by maxFails; guarded by swarm.mu
	// slots bounds this fetch's concurrent /nu/bytes requests to the peer at the server's
	// default per-peer limit, shared by small-blob and range requests, so a fetch does
	// not make the peer refuse it as busy (0084). haves does the same for /nu/have/1.
	slots, haves chan struct{}
}

// acquire takes one of p's request slots, waiting for one to free up.
func (p *peer) acquire(ctx context.Context) error {
	if p.slots == nil {
		return nil
	}
	select {
	case p.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *peer) release() {
	if p.slots != nil {
		<-p.slots
	}
}

// maxFails is how many transport failures make a peer unusable for the rest of a fetch.
const maxFails = 3

func (s *swarm) add(p Peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.index[p.ID()] == nil {
		pp := &peer{Peer: p, slots: make(chan struct{}, proto.DefaultMaxPeerRequests), haves: make(chan struct{}, proto.DefaultMaxPeerRequests)}
		s.index[p.ID()] = pp
		s.peers = append(s.peers, pp)
		s.gen++
	}
}

// generation counts the peers added so far (a change means new candidates).
func (s *swarm) generation() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

func (s *swarm) live() []*peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*peer
	for _, p := range s.peers {
		if !p.bad {
			out = append(out, p)
		}
	}
	return out
}

func (s *swarm) usable(p *peer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !p.bad
}

func (s *swarm) droppedIDs() []wire.PeerID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]wire.PeerID(nil), s.dropped...)
}

// drop excludes a peer that sent corrupt data.
func (s *swarm) drop(p *peer, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !p.bad {
		slog.Warn("fetch: dropping peer that sent corrupt data", "peer", p.ID(), "err", err)
		p.bad = true
		s.dropped = append(s.dropped, p.ID())
	}
}

// report classifies a request error. It returns true if the peer was dropped or is now
// unusable. Refusals (not found, not held) leave the peer in place.
func (s *swarm) report(ctx context.Context, p *peer, err error) bool {
	switch {
	case err == nil || ctx.Err() != nil:
		return false
	case errors.Is(err, proto.ErrCorrupt):
		s.drop(p, err)
		return true
	case errors.Is(err, proto.ErrNotFound), errors.Is(err, proto.ErrNotHeld), errors.Is(err, proto.ErrBusy):
		return false // refusals; busy is load (other clients), not a failing peer
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.bad {
		return true
	}
	if failure := connectionFailure(err); failure != nil {
		if p.connectionFailures[failure] {
			return false
		}
		if p.connectionFailures == nil {
			p.connectionFailures = make(map[*host.Conn]bool)
		}
		p.connectionFailures[failure] = true
	}
	p.fails++
	if p.fails >= maxFails {
		p.bad = true
	}
	return p.bad
}

func (r *run) timeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, r.f.cfg.RequestTimeout)
}

// whole fetches one whole object from the first live peer that has it, racing the HTTPS
// source (if any) alongside the peers.
// oneRequest is the largest blob of known size fetched whole, in one request to one peer
// (0084): one range request's worth. Spreading such blobs over peers beats splitting each,
// and it skips the have round trip and the partial blob.
func (r *run) oneRequest() uint64 { return uint64(r.f.cfg.ChunkGroups) * blob.GroupSize }

func (r *run) whole(ctx context.Context, c cid.CID) error {
	return r.wholeSized(ctx, c, 0)
}

func (r *run) wholeSized(ctx context.Context, c cid.CID, size uint64) error {
	if info, ok, err := r.st.Has(c); err != nil || (ok && info.Complete) {
		return err
	}
	return r.fetchWhole(ctx, c, size)
}

// fetchWhole is wholeSized for an object the caller found missing.
func (r *run) fetchWhole(ctx context.Context, c cid.CID, size uint64) error {
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var srcErr chan error
	if r.opts.Source != nil {
		srcErr = make(chan error, 1)
		go func() {
			err := r.opts.Source.FetchObject(ctx, c, size)
			if err == nil {
				cancel() // the peers can stop
			}
			srcErr <- err
		}()
	}
	peers := r.sw.live()
	start := int(r.next.Add(1))
	if len(peers) > 0 {
		peers = slices.Concat(peers[start%len(peers):], peers[:start%len(peers)])
	}
	peers = r.ranked(c, peers)
	var errs []error
	var putErr error
	won := false
	// A peer busy with other clients is asked again after a short wait, while only busy
	// refusals stand between this fetch and the object.
	for wait := 10 * time.Millisecond; ; wait = min(2*wait, time.Second) {
		errs = errs[:0]
		busy := false
		for _, p := range peers {
			if !r.sw.usable(p) {
				continue
			}
			if err := p.acquire(ctx); err != nil {
				break
			}
			rctx, rcancel := r.timeout(ctx)
			data, err := proto.ReadWhole(rctx, p.Open, c)
			rcancel()
			p.release() // before the store write: its sync must not idle the peer
			if err == nil {
				won = true
				cancel()                // the source can stop
				putErr = r.put(c, data) // a failure is local storage, not the peer's fault
				break
			}
			if ctx.Err() != nil {
				break // cancelled, or the source won
			}
			r.sw.report(ctx, p, err)
			busy = busy || errors.Is(err, proto.ErrBusy)
			errs = append(errs, fmt.Errorf("%s: %w", p.ID(), err))
		}
		if won || !busy || ctx.Err() != nil || slices.ContainsFunc(errs, func(e error) bool { return !errors.Is(e, proto.ErrBusy) }) {
			break
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	if srcErr != nil {
		if err := <-srcErr; err == nil {
			return nil
		} else if !won {
			errs = append(errs, err)
		}
	}
	if won {
		return putErr
	}
	if err := parent.Err(); err != nil {
		return err
	}
	if len(errs) == 0 {
		return fmt.Errorf("%w: %s: no usable peer", ErrNoProvider, c)
	}
	return fmt.Errorf("%w: %s: %w", ErrNoProvider, c, errors.Join(errs...))
}

func (r *run) wanted(c cid.CID) bool { return r.opts.Want == nil || r.opts.Want(c) }

// ranked moves the peers Rank prefers for c to the front, in Rank's order.
func (r *run) ranked(c cid.CID, peers []*peer) []*peer {
	if r.opts.Rank == nil || len(peers) < 2 {
		return peers
	}
	pos := map[wire.PeerID]int{}
	for i, id := range r.opts.Rank(c) {
		if _, ok := pos[id]; !ok {
			pos[id] = i
		}
	}
	out := slices.Clone(peers)
	slices.SortStableFunc(out, func(a, b *peer) int {
		ia, oka := pos[a.ID()]
		ib, okb := pos[b.ID()]
		switch {
		case oka && okb:
			return ia - ib
		case oka:
			return -1
		case okb:
			return 1
		}
		return 0
	})
	return out
}

// Link is a link of a structure node; Size is the blob size its entry declares (0 for
// structure links and unknown sizes).
type Link struct {
	CID  cid.CID
	Size uint64
}

// Links returns the nu-tree links of a structure node in walk order, with declared blob
// sizes.
func Links(c cid.CID, data []byte) ([]Link, error) {
	refs, err := children(c, data)
	out := make([]Link, len(refs))
	for i, r := range refs {
		out[i] = Link{CID: r.c, Size: r.size}
	}
	return out, err
}

// blobRef is a blob reached by the walk, with its size from an authenticated parent.
type blobRef struct {
	c    cid.CID
	size uint64
}

// walk fetches the structure level by level, then the blobs.
func (r *run) walk(ctx context.Context, root cid.CID, size uint64) error {
	// Keyed by full CID: the same bytes linked as a raw blob and as a DRISL node are two
	// objects to fetch (0079; the store holds a DRISL read only for bytes stored as DRISL).
	seen := map[cid.CID]bool{root: true}
	links := map[cid.CID][]blobRef{} // each structure node's links, for the blob order
	var blobs []blobRef
	var frontier []cid.CID
	if root.Codec() == cid.Raw {
		if r.wanted(root) {
			blobs = append(blobs, blobRef{root, size})
		}
	} else {
		frontier = append(frontier, root)
	}
	r.update(func(p *Progress) { p.ObjectsTotal++; p.BytesTotal += int64(size) })
	if len(frontier) > 0 && !r.opts.Catalog {
		r.stage(StageStructure)
		r.treeSync(ctx, root)
	}
	for len(frontier) > 0 {
		r.stage(StageStructure)
		// One batched lookup per level: after treeSync most of the level is held.
		held, err := r.st.GetMany(frontier)
		if err != nil {
			return err
		}
		var missing []cid.CID
		for _, c := range frontier {
			if _, ok := held[c]; !ok {
				missing = append(missing, c)
			}
		}
		r.update(func(p *Progress) { p.Objects += int64(len(frontier) - len(missing)) })
		if err := parallel(ctx, missing, r.f.cfg.Structure, func(ctx context.Context, c cid.CID) error {
			if err := r.whole(ctx, c); err != nil {
				return err
			}
			r.update(func(p *Progress) { p.Objects++ })
			return nil
		}); err != nil {
			return err
		}
		var next []cid.CID
		for _, c := range frontier {
			data, ok := held[c]
			if !ok {
				if data, err = r.st.Get(c); err != nil {
					return err
				}
			}
			kids, err := r.children(c, data)
			if err != nil {
				return fmt.Errorf("fetch: walking %s: %w", c, err)
			}
			links[c] = kids
			for _, k := range kids {
				if seen[k.c] || (r.opts.Skip != nil && r.opts.Skip(k.c)) {
					continue
				}
				seen[k.c] = true
				r.update(func(p *Progress) { p.ObjectsTotal++; p.BytesTotal += int64(k.size) })
				if k.c.Codec() == cid.Raw {
					if r.wanted(k.c) {
						blobs = append(blobs, k)
					}
				} else {
					next = append(next, k.c)
				}
			}
		}
		frontier = next
	}
	if len(blobs) == 0 {
		return nil
	}
	blobs = depthFirst(root, links, blobs)
	r.stage(StageBlobs)
	// Blobs already held (from treeSync or earlier fetches) are counted, not fetched; one
	// batched lookup instead of one per blob.
	cs := make([]cid.CID, len(blobs))
	for i, b := range blobs {
		cs[i] = b.c
	}
	held, err := r.st.HasMany(cs)
	if err != nil {
		return err
	}
	var heldBytes, heldObjects int64
	rest := blobs[:0]
	for _, b := range blobs {
		if info, ok := held[b.c]; ok && info.Complete {
			heldBytes += int64(info.Size)
			heldObjects++
			continue
		}
		rest = append(rest, b)
	}
	blobs = rest
	r.update(func(p *Progress) { p.Bytes += heldBytes; p.Objects += heldObjects })
	// Large blobs each use every peer; small ones (at most one range request) are one
	// request each and run 16× wider, which also lets their commit fsyncs share flushes.
	var small, large []blobRef
	for _, b := range blobs {
		if b.c.Hash() == cid.BLAKE3 && (b.size == 0 || b.size > r.oneRequest()) {
			large = append(large, b)
		} else {
			small = append(small, b)
		}
	}
	fetchOne := func(ctx context.Context, b blobRef) error {
		if err := r.blob(ctx, b.c, b.size); err != nil {
			return err
		}
		r.update(func(p *Progress) { p.Objects++ })
		return nil
	}
	// A blob nobody serves does not stop the others: every available blob is stored
	// (a later fetch, or a placement retry, then has only the missing ones to ask for).
	// Small blobs are held in memory between their request and their (syncing) store
	// write, so their count is bounded by bytes too: 16 MiB per Blobs, at most 64×Blobs.
	errSmall := make(chan error, 1)
	smallWeight := func(b blobRef) int64 { return int64(max(b.size, 16<<10)) }
	go func() {
		errSmall <- parallelWeighted(ctx, small, int64(r.f.cfg.Blobs)<<24, 64*r.f.cfg.Blobs, smallWeight, fetchOne)
	}()
	// Large blobs share the run's per-peer range workers (lanes), which need enough open
	// blobs to keep every peer busy: many when they are a few chunks each, a few when big.
	const maxWeight = 64 << 20
	weight := func(b blobRef) int64 {
		if b.size == 0 {
			return maxWeight
		}
		return int64(min(max(b.size, r.oneRequest()), maxWeight))
	}
	errLarge := parallelWeighted(ctx, large, int64(r.f.cfg.Blobs)*maxWeight, 16*r.f.cfg.Blobs, weight, fetchOne)
	return errors.Join(errLarge, <-errSmall)
}

// depthFirst orders blobs (found level by level) as a depth-first walk of the structure
// in link order meets them: for a tree that is path order, the order `get` writes files
// in, so files complete in the order they are written out.
func depthFirst(root cid.CID, links map[cid.CID][]blobRef, blobs []blobRef) []blobRef {
	want := make(map[cid.CID]blobRef, len(blobs))
	for _, b := range blobs {
		want[b.c] = b
	}
	out := make([]blobRef, 0, len(blobs))
	visited := map[cid.CID]bool{}
	var visit func(c cid.CID)
	visit = func(c cid.CID) {
		for _, k := range links[c] {
			if visited[k.c] {
				continue
			}
			visited[k.c] = true
			if b, ok := want[k.c]; ok {
				out = append(out, b)
			} else {
				visit(k.c)
			}
		}
	}
	if b, ok := want[root]; ok {
		out = append(out, b)
	}
	visit(root)
	if len(out) != len(blobs) { // not reachable through recorded links: keep the walk's order
		return blobs
	}
	return out
}

func (r *run) children(c cid.CID, data []byte) ([]blobRef, error) {
	if r.opts.Profile == "atrepo" || r.opts.Profile == "atrepo+blobs" {
		links, err := AtrepoLinks(c, data, r.opts.Profile == "atrepo+blobs")
		out := make([]blobRef, len(links))
		for i, l := range links {
			out[i] = blobRef{l.CID, l.Size}
		}
		return out, err
	}
	if !r.opts.Catalog {
		return children(c, data)
	}
	links, err := tree.CatalogLinks(c, data)
	out := make([]blobRef, len(links))
	for i, l := range links {
		out[i] = blobRef{c: l}
	}
	return out, err
}

// children returns the nu-tree links of a structure node with sizes for blob links.
func children(c cid.CID, data []byte) ([]blobRef, error) {
	v, err := drisl.Decode(data)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	switch {
	case m != nil && m["$type"] == tree.TypeMASL && m["src"] != nil:
		fn, err := tree.DecodeFileNode(data)
		if err != nil {
			return nil, err
		}
		return []blobRef{{fn.Src, fn.Nu.Size}}, nil
	case m != nil && m["$type"] == nil && m["e"] != nil:
		n, err := tree.DirProfile.Codec.Decode(data)
		if err != nil {
			return nil, err
		}
		var out []blobRef
		if n.Left.Defined() {
			out = append(out, blobRef{c: n.Left})
		}
		for _, e := range n.Entries {
			if err := e.Val.Validate(); err != nil {
				return nil, err
			}
			ref := blobRef{c: e.Val.CID}
			if e.Val.Kind != tree.KindDir {
				ref.size = e.Val.Size
			}
			out = append(out, ref)
			if e.Val.Meta.Defined() {
				out = append(out, blobRef{c: e.Val.Meta})
			}
			if e.Right.Defined() {
				out = append(out, blobRef{c: e.Right})
			}
		}
		return out, nil
	}
	links, err := tree.Links(c, data)
	out := make([]blobRef, len(links))
	for i, l := range links {
		out[i] = blobRef{c: l}
	}
	return out, err
}

// parallelWeighted runs fn over every item (ctx still stops it) and joins the errors.
// Items run while their weights fit budget (an item heavier than budget runs alone) and
// at most n at a time.
func parallelWeighted[T any](ctx context.Context, items []T, budget int64, n int, weight func(T) int64, fn func(context.Context, T) error) error {
	var mu sync.Mutex
	cond := sync.NewCond(&mu)
	stop := context.AfterFunc(ctx, func() { mu.Lock(); cond.Broadcast(); mu.Unlock() })
	defer stop()
	var used int64
	running := 0
	var wg sync.WaitGroup
	var errs []error
	for _, it := range items {
		w := weight(it)
		mu.Lock()
		for ctx.Err() == nil && running > 0 && (running >= n || used+w > budget) {
			cond.Wait()
		}
		if ctx.Err() != nil {
			mu.Unlock()
			break
		}
		used += w
		running++
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := fn(ctx, it)
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
			}
			used -= w
			running--
			cond.Broadcast()
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return ctx.Err()
}

// parallel runs fn over items with at most n at a time, stopping at the first error.
func parallel[T any](ctx context.Context, items []T, n int, fn func(context.Context, T) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	for _, it := range items {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, it); err != nil {
				once.Do(func() { first = err; cancel() })
			}
		}()
	}
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}
