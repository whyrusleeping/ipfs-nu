package nutest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/proto"
)

// Opt-in diagnostics, outside production: request timing/bytes, progress samples,
// and bounded goroutine dumps when progress stops for >1s OR throughput is low
// for two consecutive seconds. Repeated sub-second sync stalls otherwise evade
// an idle-only watchdog. NU_DOWNLOAD_TRACE must be an absolute parent directory;
// every iteration gets its own dir. Use -blockprofile to enable block snapshots.
type downloadTrace struct {
	start      time.Time
	dir        string
	file       *os.File
	mu         sync.Mutex
	enc        *json.Encoder
	active     map[*tracedStream]bool
	bytes      atomic.Int64
	last       atomic.Int64
	stop, done chan struct{}
	closeOnce  sync.Once
	minRate    int64
}

func newDownloadTrace(b *testing.B, peers []fetch.Peer, minRate int64) (*downloadTrace, []fetch.Peer) {
	b.Helper()
	parent := os.Getenv("NU_DOWNLOAD_TRACE")
	if parent == "" {
		return nil, peers
	}
	if !filepath.IsAbs(parent) {
		b.Fatal("NU_DOWNLOAD_TRACE must be absolute (tests run in their package directory)")
	}
	if err := os.MkdirAll(parent, 0700); err != nil {
		b.Fatal(err)
	}
	dir, err := os.MkdirTemp(parent, "transfer-")
	if err != nil {
		b.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		b.Fatal(err)
	}
	d := &downloadTrace{start: time.Now(), dir: dir, file: f, enc: json.NewEncoder(f), active: map[*tracedStream]bool{}, stop: make(chan struct{}), done: make(chan struct{})}
	d.minRate = minRate
	d.event(map[string]any{"event": "start", "benchmark": b.Name(), "min_bytes_per_second": minRate})
	out := make([]fetch.Peer, len(peers))
	for i, p := range peers {
		out[i] = tracedPeer{Peer: p, d: d}
	}
	go d.watch()
	b.Cleanup(d.close)
	b.Log("diagnostics:", dir)
	return d, out
}

func (d *downloadTrace) event(v map[string]any) {
	v["seconds"] = time.Since(d.start).Seconds()
	d.mu.Lock()
	_ = d.enc.Encode(v)
	d.mu.Unlock()
}
func (d *downloadTrace) progress(p fetch.Progress) {
	if d == nil {
		return
	}
	if p.Bytes != d.bytes.Swap(p.Bytes) {
		d.last.Store(time.Since(d.start).Nanoseconds())
	}
}
func (d *downloadTrace) close() {
	if d == nil {
		return
	}
	d.closeOnce.Do(func() {
		close(d.stop)
		<-d.done
		d.event(map[string]any{"event": "done", "bytes": d.bytes.Load()})
		d.file.Close()
	})
}
func (d *downloadTrace) watch() {
	defer close(d.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	dumps, slow := 0, 0
	var prev int64
	lastTick := time.Now()
	for {
		select {
		case <-d.stop:
			return
		case <-ticker.C:
			idle := time.Since(d.start) - time.Duration(d.last.Load())
			var active []map[string]any
			d.mu.Lock()
			for s := range d.active {
				active = append(active, s.snapshot())
			}
			d.mu.Unlock()
			current := d.bytes.Load()
			rate := float64(current-prev) / time.Since(lastTick).Seconds()
			prev, lastTick = current, time.Now()
			if rate < float64(d.minRate) {
				slow++
			} else {
				slow = 0
			}
			d.event(map[string]any{"event": "progress", "bytes": current, "bytes_per_second": rate, "idle_seconds": idle.Seconds(), "active": active})
			if (idle > time.Second || slow >= 2) && dumps < 12 {
				if dumps == 0 {
					if f, err := os.Create(filepath.Join(d.dir, "stall.block")); err == nil {
						_ = pprof.Lookup("block").WriteTo(f, 0)
						f.Close()
					}
				}
				path := filepath.Join(d.dir, fmt.Sprintf("stall-%02d.txt", dumps))
				if f, err := os.Create(path); err == nil {
					_ = pprof.Lookup("goroutine").WriteTo(f, 2)
					f.Close()
				}
				dumps++
			}
		}
	}
}

type tracedPeer struct {
	fetch.Peer
	d *downloadTrace
}

func (p tracedPeer) Open(ctx context.Context, protocol string) (proto.Stream, error) {
	start := time.Now()
	s, err := p.Peer.Open(ctx, protocol)
	if err != nil {
		p.d.event(map[string]any{"event": "open_error", "error": err.Error(), "peer": p.ID().String(), "duration": time.Since(start).Seconds()})
		return nil, err
	}
	ts := &tracedStream{Stream: s, d: p.d, peer: p.ID().String(), protocol: protocol, start: start}
	p.d.mu.Lock()
	p.d.active[ts] = true
	p.d.mu.Unlock()
	return ts, nil
}

type tracedStream struct {
	proto.Stream
	d                       *downloadTrace
	peer, protocol          string
	start                   time.Time
	bytes, reading, maxRead atomic.Int64
	once                    sync.Once
}

func (s *tracedStream) Read(p []byte) (int, error) {
	start := time.Now()
	s.reading.Store(start.UnixNano())
	n, err := s.Stream.Read(p)
	s.reading.Store(0)
	s.bytes.Add(int64(n))
	elapsed := time.Since(start).Nanoseconds()
	if elapsed > s.maxRead.Load() {
		s.maxRead.Store(elapsed)
	}
	return n, err
}
func (s *tracedStream) snapshot() map[string]any {
	waiting := float64(0)
	if at := s.reading.Load(); at != 0 {
		waiting = time.Since(time.Unix(0, at)).Seconds()
	}
	return map[string]any{"peer": s.peer, "protocol": s.protocol, "duration": time.Since(s.start).Seconds(), "bytes": s.bytes.Load(), "reading_seconds": waiting, "max_read_seconds": float64(s.maxRead.Load()) / 1e9}
}
func (s *tracedStream) finish(reason string) {
	s.once.Do(func() {
		v := s.snapshot()
		v["event"] = reason
		s.d.mu.Lock()
		delete(s.d.active, s)
		s.d.mu.Unlock()
		s.d.event(v)
	})
}
func (s *tracedStream) Close() error { err := s.Stream.Close(); s.finish("close"); return err }
func (s *tracedStream) Reset()       { s.Stream.Reset(); s.finish("reset") }
