package fetch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
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
	// PeerRequests is the number of concurrent range requests per peer for one blob (4).
	PeerRequests int
	// ChunkGroups is the size of a blob range request in 16 KiB groups (64, i.e. 1 MiB).
	ChunkGroups int
	// Structure is the number of structure nodes fetched in parallel per level (16).
	Structure int
	// Blobs is the number of blobs fetched in parallel (4 large, 16× that for small ones).
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

// Fetch fetches c and everything its nu-tree walk reaches (header → directories → entries →
// blobs; MASL file node → blob; a raw blob alone). Structure and small blobs come first over
// /nu/tree/1 from peers that support it; then a layer-1 walk fetches what is still missing
// level by level over /nu/bytes/1, and the blobs in parallel across peers. Objects already
// held are not fetched again, and partial blobs resume from their verified groups.
func (f *Fetcher) Fetch(ctx context.Context, c cid.CID, o Options) (Result, error) {
	r := &run{f: f, st: f.cfg.Store, opts: o, sw: &swarm{index: map[wire.PeerID]*peer{}}}
	r.stage(StageProviders)
	for _, p := range o.Peers {
		r.sw.add(p)
	}
	if f.cfg.Providers != nil {
		if err := f.cfg.Providers.Find(ctx, c, r.sw.add); err != nil && len(r.sw.live()) == 0 && o.Source == nil {
			return r.result(), err
		}
	}
	err := r.walk(ctx, c, o.Size)
	if err == nil {
		r.stage(StageDone)
	}
	return r.result(), err
}

// run is one Fetch call.
type run struct {
	f    *Fetcher
	st   *store.Store
	opts Options
	sw   *swarm
	next atomic.Uint64 // rotates whole-object requests across peers

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
}

type peer struct {
	Peer
	bad   bool // dropped (corrupt) or failing; guarded by swarm.mu
	fails int
}

// maxFails is how many transport failures make a peer unusable for the rest of a fetch.
const maxFails = 3

func (s *swarm) add(p Peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.index[p.ID()] == nil {
		pp := &peer{Peer: p}
		s.index[p.ID()] = pp
		s.peers = append(s.peers, pp)
	}
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
	case errors.Is(err, proto.ErrNotFound), errors.Is(err, proto.ErrNotHeld):
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
func (r *run) whole(ctx context.Context, c cid.CID) error {
	return r.wholeSized(ctx, c, 0)
}

func (r *run) wholeSized(ctx context.Context, c cid.CID, size uint64) error {
	if info, ok, err := r.st.Has(c); err != nil || (ok && info.Complete) {
		return err
	}
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
	var errs []error
	won := false
	for i := range peers {
		p := peers[(start+i)%len(peers)]
		if !r.sw.usable(p) {
			continue
		}
		rctx, rcancel := r.timeout(ctx)
		err := proto.FetchWhole(rctx, p.Open, r.st, c)
		rcancel()
		if err == nil {
			won = true
			cancel() // the source can stop
			break
		}
		if ctx.Err() != nil {
			break // cancelled, or the source won
		}
		r.sw.report(ctx, p, err)
		errs = append(errs, fmt.Errorf("%s: %w", p.ID(), err))
	}
	if srcErr != nil {
		if err := <-srcErr; err == nil {
			return nil
		} else if !won {
			errs = append(errs, err)
		}
	}
	if won {
		return nil
	}
	if err := parent.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s: %w", ErrNoProvider, c, errors.Join(errs...))
}

// blobRef is a blob reached by the walk, with its size from an authenticated parent.
type blobRef struct {
	c    cid.CID
	size uint64
}

// walk fetches the structure level by level, then the blobs.
func (r *run) walk(ctx context.Context, root cid.CID, size uint64) error {
	seen := map[cid.Multihash]bool{root.Multihash(): true}
	var blobs []blobRef
	var frontier []cid.CID
	if root.Codec() == cid.Raw {
		blobs = append(blobs, blobRef{root, size})
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
		if err := parallel(ctx, frontier, r.f.cfg.Structure, func(ctx context.Context, c cid.CID) error {
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
			data, err := r.st.Get(c)
			if err != nil {
				return err
			}
			kids, err := r.children(c, data)
			if err != nil {
				return fmt.Errorf("fetch: walking %s: %w", c, err)
			}
			for _, k := range kids {
				if seen[k.c.Multihash()] {
					continue
				}
				seen[k.c.Multihash()] = true
				r.update(func(p *Progress) { p.ObjectsTotal++; p.BytesTotal += int64(k.size) })
				if k.c.Codec() == cid.Raw {
					blobs = append(blobs, k)
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
	r.stage(StageBlobs)
	// Large blobs each use every peer; small ones are one request each.
	var small, large []blobRef
	for _, b := range blobs {
		if b.c.Hash() == cid.BLAKE3 && (b.size == 0 || blob.HasOutboard(b.size)) {
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
	errSmall := make(chan error, 1)
	go func() { errSmall <- parallel(ctx, small, 16*r.f.cfg.Blobs, fetchOne) }()
	return errors.Join(parallel(ctx, large, r.f.cfg.Blobs, fetchOne), <-errSmall)
}

func (r *run) children(c cid.CID, data []byte) ([]blobRef, error) {
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
