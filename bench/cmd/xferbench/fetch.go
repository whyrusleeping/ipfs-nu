package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"runtime/pprof"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/whyrusleeping/nu/core/blob"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/node/link"
	"github.com/whyrusleeping/nu/node/swarm"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/proto"
	"github.com/whyrusleeping/nu/store"
)

type fetchOpts struct {
	mode       string
	repeat     int
	perPeer    int
	chunk      uint64
	storeRoot  string
	cpuprofile string
}

func fetchCmd(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("fetch", flag.ExitOnError)
	var o fetchOpts
	fl.StringVar(&o.mode, "mode", "discard", "discard (verify and drop) or fetcher (Nu's fetcher into a fresh store)")
	fl.IntVar(&o.repeat, "repeat", 1, "runs (the summary is their median)")
	fl.IntVar(&o.perPeer, "peer-requests", proto.DefaultMaxPeerRequests, "discard: concurrent requests per peer")
	fl.Uint64Var(&o.chunk, "chunk", proto.MaxRanges, "discard: groups (16 KiB) per request")
	fl.StringVar(&o.storeRoot, "store", "", "fetcher: directory for the per-run stores (default tmpfs when present)")
	fl.StringVar(&o.cpuprofile, "cpuprofile", "", "write a CPU profile of the runs")
	fl.Parse(args)
	if fl.NArg() != 1 {
		usage()
	}
	if o.mode != "discard" && o.mode != "fetcher" {
		return fmt.Errorf("unknown -mode %q", o.mode)
	}
	l, err := link.Parse(fl.Arg(0))
	if err != nil {
		return err
	}
	if len(l.Providers) == 0 {
		return errors.New("the link names no provider (peer=/addr=)")
	}

	// The swarm a daemonless `nu get` starts, without the DHT; its store holds the
	// structure for discard runs.
	dir, err := tempStoreDir(o.storeRoot, "xferbench-fetch-")
	if err != nil {
		return err
	}
	defer removeAll(dir)
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		return err
	}
	defer st.Close()
	key, err := wire.GenerateNodeKey()
	if err != nil {
		return err
	}
	sw, err := swarm.Start(ctx, swarm.Config{Key: key, Store: st, Logger: slog.New(slog.DiscardHandler),
		Listen: []string{"/ip4/0.0.0.0/udp/0/quic-v1"}, DHT: swarm.DHTConfig{Mode: "off"}})
	if err != nil {
		return err
	}
	defer sw.Close()
	var hints []lookup.Hint
	for _, p := range l.Providers {
		if err := sw.Connect(ctx, p.Peer, p.Addrs); err != nil {
			return fmt.Errorf("connecting to %s: %w", p.Peer, err)
		}
		hints = append(hints, lookup.Hint{Peer: p.Peer, Addresses: p.Addrs})
	}
	var peers []fetch.Peer
	if err := (fetch.Hinted{Sessions: sw.Sessions(), Hints: hints}).Find(ctx, l.CID, func(p fetch.Peer) { peers = append(peers, p) }); err != nil {
		return err
	}

	var items []item
	var blobs, total uint64
	if o.mode == "discard" {
		f := fetch.New(fetch.Config{Store: st})
		if _, err := f.Fetch(ctx, l.CID, fetch.Options{Peers: peers, Want: func(cid.CID) bool { return false }}); err != nil {
			return fmt.Errorf("fetching structure: %w", err)
		}
		bl, err := walk(ctx, st, peers[0], l.CID)
		if err != nil {
			return err
		}
		for _, b := range bl {
			if b.CID.Hash() != cid.BLAKE3 {
				return fmt.Errorf("blob %s is not BLAKE3; discard mode measures range transfers only", b.CID)
			}
			g := blob.Groups(b.Size)
			for s := uint64(0); s < g; s += o.chunk {
				r := blob.Range{Start: s, End: min(s+o.chunk, g)}
				_, n := r.Bytes(b.Size)
				items = append(items, item{mh: b.CID.Multihash(), size: b.Size, r: r, bytes: n})
			}
			total += b.Size
		}
		blobs = uint64(len(bl))
	}

	if o.cpuprofile != "" {
		pf, err := os.Create(o.cpuprofile)
		if err != nil {
			return err
		}
		defer pf.Close()
		if err := pprof.StartCPUProfile(pf); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
	}
	var rates []float64
	corrupt := 0
	for run := 1; run <= o.repeat; run++ {
		cpu := startCPU()
		var line string
		var bytes uint64
		if o.mode == "discard" {
			sent, bad, err := discard(ctx, peers, items, o.perPeer)
			if err != nil {
				return err
			}
			corrupt += bad
			line = fmt.Sprintf("%d blobs, %d requests, %d corrupt", blobs, len(items), bad)
			for i, p := range peers {
				line += fmt.Sprintf(" %s=%dMiB", p.ID().String()[:12], sent[i]>>20)
			}
			bytes = total
		} else {
			res, err := fetcherRun(ctx, peers, l.CID, o.storeRoot)
			if err != nil {
				return err
			}
			blobs, bytes = uint64(res.Objects), uint64(res.Bytes)
			line = fmt.Sprintf("%d objects (structure included)", res.Objects)
		}
		el := time.Since(cpu.wall)
		rate := float64(bytes) / 1e6 / el.Seconds()
		rates = append(rates, rate)
		fmt.Printf("xfer %s run %d: %d bytes in %.3fs = %.0f MB/s, %.0f objects/s, %.1f cores | %s\n",
			o.mode, run, bytes, el.Seconds(), rate, float64(blobs)/el.Seconds(), cpu.cores(), line)
	}
	slices.Sort(rates)
	fmt.Printf("xfer %s median of %d: %.0f MB/s (min %.0f, max %.0f) from %d peers\n",
		o.mode, len(rates), rates[len(rates)/2], rates[0], rates[len(rates)-1], len(peers))
	if corrupt > 0 {
		return fmt.Errorf("%d responses failed verification (a bug: the servers are honest)", corrupt)
	}
	return nil
}

type item struct {
	mh    cid.Multihash
	size  uint64
	r     blob.Range
	bytes uint64
}

// corruptLimit is how often one item may fail verification before discard gives up.
const corruptLimit = 3

// discard requests every item from one shared queue, perPeer at a time per peer, and
// returns the bytes each peer delivered. A response that fails verification is reported
// on stderr and requested again (honest servers never send one, so any count is a bug).
func discard(ctx context.Context, peers []fetch.Peer, items []item, perPeer int) ([]uint64, int, error) {
	queue := make(chan item)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer close(queue)
		for _, it := range items {
			select {
			case queue <- it:
			case <-ctx.Done():
				return
			}
		}
	}()
	sent := make([]atomic.Uint64, len(peers))
	var corrupt atomic.Int64
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	for i, p := range peers {
		for range perPeer {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for it := range queue {
					bad := 0
					for wait := time.Millisecond; ; wait = min(2*wait, 100*time.Millisecond) {
						err := proto.ReadRanges(ctx, p.Open, it.mh, it.size, []blob.Range{it.r}, blob.Sink{})
						if errors.Is(err, proto.ErrBusy) {
							time.Sleep(wait)
							continue
						}
						if errors.Is(err, proto.ErrCorrupt) && bad < corruptLimit {
							bad++
							corrupt.Add(1)
							fmt.Fprintf(os.Stderr, "xferbench: CORRUPT response from %s for blob %s (%d bytes) groups %d-%d: %v\n",
								p.ID(), it.mh, it.size, it.r.Start, it.r.End, err)
							continue
						}
						if err != nil {
							once.Do(func() { firstErr = fmt.Errorf("%s: %w", p.ID(), err); cancel() })
							return
						}
						sent[i].Add(it.bytes)
						break
					}
				}
			}()
		}
	}
	wg.Wait()
	out := make([]uint64, len(peers))
	for i := range sent {
		out[i] = sent[i].Load()
	}
	if firstErr != nil {
		return nil, int(corrupt.Load()), firstErr
	}
	return out, int(corrupt.Load()), ctx.Err()
}

// fetcherRun fetches root with Nu's fetcher into a fresh store, then removes it.
func fetcherRun(ctx context.Context, peers []fetch.Peer, root cid.CID, storeRoot string) (fetch.Result, error) {
	dir, err := tempStoreDir(storeRoot, "xferbench-run-")
	if err != nil {
		return fetch.Result{}, err
	}
	defer removeAll(dir)
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		return fetch.Result{}, err
	}
	defer st.Close()
	res, err := fetch.New(fetch.Config{Store: st}).Fetch(ctx, root, fetch.Options{Peers: peers})
	if err != nil {
		return res, err
	}
	if len(res.Dropped) > 0 {
		return res, fmt.Errorf("peers sent corrupt data: %v", res.Dropped)
	}
	return res, nil
}

// walk lists the blobs under root from the structure in st, with their declared sizes. A
// raw root has no parent to declare its size, so its size is asked of a peer (a wrong claim
// fails Bao verification).
func walk(ctx context.Context, st *store.Store, p fetch.Peer, root cid.CID) ([]fetch.Link, error) {
	if root.Codec() == cid.Raw {
		res, err := proto.Have(ctx, p.Open, []proto.HaveQuery{{MH: root.Multihash()}})
		if err != nil {
			return nil, err
		}
		if !res[0].Held {
			return nil, fmt.Errorf("%s does not hold %s", p.ID(), root)
		}
		return []fetch.Link{{CID: root, Size: res[0].Size}}, nil
	}
	seen := map[cid.CID]bool{root: true}
	var blobs []fetch.Link
	frontier := []cid.CID{root}
	for len(frontier) > 0 {
		var next []cid.CID
		for _, c := range frontier {
			data, err := st.Get(c)
			if err != nil {
				return nil, err
			}
			links, err := fetch.Links(c, data)
			if err != nil {
				return nil, err
			}
			for _, k := range links {
				if seen[k.CID] {
					continue
				}
				seen[k.CID] = true
				if k.CID.Codec() == cid.Raw {
					blobs = append(blobs, k)
				} else {
					next = append(next, k.CID)
				}
			}
		}
		frontier = next
	}
	return blobs, nil
}
