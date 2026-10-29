// Command xferbench measures the transfer alone: it fetches a share link's tree structure
// into a temporary store, then requests every blob's bytes from all hinted peers over
// /nu/bytes/1 and Bao-verifies them into a sink that discards them. Nothing is written to
// disk, so comparing it with `nu get` separates the network, server and verification from
// the recipient's fetch/store pipeline (scripts/lan-bench.sh xfer1/xfer2).
//
//	xferbench [-peer-requests 16] [-chunk 64] [-cpuprofile f] 'nu://<cid>?peer=…&addr=…'
//
// Every peer gets -peer-requests workers taking requests from one shared queue in walk
// order: a blob of at most -chunk groups is one request, larger blobs one per -chunk groups.
// It prints one summary line.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/pprof"
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

func main() {
	perPeer := flag.Int("peer-requests", proto.DefaultMaxPeerRequests, "concurrent requests per peer")
	chunk := flag.Uint64("chunk", proto.MaxRanges, "groups (16 KiB) per request")
	cpuprofile := flag.String("cpuprofile", "", "write a CPU profile of the transfer phase")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: xferbench [flags] <nu:// link with peer hints>")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, flag.Arg(0), *perPeer, *chunk, *cpuprofile); err != nil {
		fmt.Fprintln(os.Stderr, "xferbench:", err)
		os.Exit(1)
	}
}

type item struct {
	mh    cid.Multihash
	size  uint64
	r     blob.Range
	bytes uint64
}

func run(ctx context.Context, raw string, perPeer int, chunk uint64, cpuprofile string) error {
	l, err := link.Parse(raw)
	if err != nil {
		return err
	}
	if len(l.Providers) == 0 {
		return errors.New("the link names no provider (peer=/addr=)")
	}
	dir, err := os.MkdirTemp("", "xferbench-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		return err
	}
	defer st.Close()
	key, err := wire.GenerateNodeKey()
	if err != nil {
		return err
	}
	// The same swarm a daemonless `nu get` starts, without the DHT.
	cfg := swarm.Config{Key: key, Store: st, Logger: slog.New(slog.DiscardHandler),
		Listen: []string{"/ip4/0.0.0.0/udp/0/quic-v1"}, DHT: swarm.DHTConfig{Mode: "off"}}
	sw, err := swarm.Start(ctx, cfg)
	if err != nil {
		return err
	}
	defer sw.Close()

	t0 := time.Now()
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
	tConn := time.Now()

	// Structure only (no blob is wanted), then the blobs it names, in walk order.
	f := fetch.New(fetch.Config{Store: st})
	if _, err := f.Fetch(ctx, l.CID, fetch.Options{Peers: peers, Want: func(cid.CID) bool { return false }}); err != nil {
		return fmt.Errorf("fetching structure: %w", err)
	}
	blobs, err := walk(ctx, st, peers[0], l.CID)
	if err != nil {
		return err
	}
	tStruct := time.Now()

	var items []item
	var total uint64
	for _, b := range blobs {
		if b.CID.Hash() != cid.BLAKE3 {
			return fmt.Errorf("blob %s is not BLAKE3; xferbench measures range transfers only", b.CID)
		}
		g := blob.Groups(b.Size)
		for s := uint64(0); s < g; s += chunk {
			r := blob.Range{Start: s, End: min(s+chunk, g)}
			_, n := r.Bytes(b.Size)
			items = append(items, item{mh: b.CID.Multihash(), size: b.Size, r: r, bytes: n})
		}
		total += b.Size
	}

	if cpuprofile != "" {
		pf, err := os.Create(cpuprofile)
		if err != nil {
			return err
		}
		defer pf.Close()
		if err := pprof.StartCPUProfile(pf); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
	}
	queue := make(chan item)
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sent := make([]atomic.Uint64, len(peers))
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	for i, p := range peers {
		for range perPeer {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for it := range queue {
					for wait := time.Millisecond; ; wait = min(2*wait, 100*time.Millisecond) {
						err := proto.ReadRanges(ctx, p.Open, it.mh, it.size, []blob.Range{it.r}, blob.Sink{})
						if errors.Is(err, proto.ErrBusy) {
							time.Sleep(wait)
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
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	el := time.Since(tStruct)
	fmt.Printf("xfer: %d blobs, %d bytes in %.3fs = %.0f MB/s, %.0f blobs/s, %d requests | connect %.3fs structure %.3fs | per peer:",
		len(blobs), total, el.Seconds(), float64(total)/1e6/el.Seconds(), float64(len(blobs))/el.Seconds(), len(items),
		tConn.Sub(t0).Seconds(), tStruct.Sub(tConn).Seconds())
	for i, p := range peers {
		fmt.Printf(" %s=%dMiB", p.ID().String()[:12], sent[i].Load()>>20)
	}
	fmt.Println()
	return nil
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
