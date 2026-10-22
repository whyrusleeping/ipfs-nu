package replica

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

func peer(t *testing.T, i uint64) wire.PeerID {
	var seed [ed25519.SeedSize]byte
	binary.LittleEndian.PutUint64(seed[:], i)
	k, err := wire.ParseNodeKey(wire.AppendBytes(wire.AppendUint(nil, 1, uint64(wire.Ed25519)), 2, ed25519.NewKeyFromSeed(seed[:])), wire.MaxKeyBytes)
	if err != nil {
		t.Fatal(err)
	}
	return k.Public().PeerID()
}

type fixture struct {
	st   *store.Store
	c    *cluster.Cluster
	m    *Manager
	self wire.PeerID
}

// newFixture is a private cluster founded here with a second storage member that never
// reports, so a replicas-2 dataset is never policy-ready.
func newFixture(t *testing.T, fetch FetchFunc) *fixture {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	self := peer(t, 1)
	c, err := cluster.Create(cluster.Options{Dir: filepath.Join(dir, "c"), Store: st}, "lab", cluster.ModePrivate, self, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(
		cluster.Op{Collection: cluster.NSMember, RKey: peer(t, 2).String(), Value: &cluster.Member{Roles: []string{cluster.RoleStorage}, State: cluster.StateActive}},
		cluster.Op{Collection: cluster.NSDataset, RKey: "ds", Value: &cluster.Dataset{Source: cluster.Source{Local: true}, Policy: cluster.Policy{Replicas: 2}}},
	); err != nil {
		t.Fatal(err)
	}
	m, err := Open(Config{Store: st, Cluster: c, Self: self, Dir: filepath.Join(dir, "c"), Fetch: fetch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return &fixture{st: st, c: c, m: m, self: self}
}

func (f *fixture) head(t *testing.T, target cid.CID) {
	t.Helper()
	if _, err := f.c.Write(cluster.Op{Collection: cluster.NSHead, RKey: "ds", Value: &cluster.Head{Target: target.String(), Kind: "blob"}}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) pinned(t *testing.T) []string {
	t.Helper()
	pins, err := f.st.Pins()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range pins {
		if p.Source == store.SourceCluster && p.Name == "dataset/ds" {
			out = append(out, p.CID.String())
		}
	}
	slices.Sort(out)
	return out
}

func (f *fixture) has(c cid.CID) bool {
	info, ok, err := f.st.Has(c)
	return err == nil && ok && info.Complete
}

func randBlob(seed uint64, n int) []byte {
	b := make([]byte, n)
	rand.NewChaCha8([32]byte{byte(seed)}).Read(b)
	return b
}

// waitFetches lets the reconcile's fetch goroutines finish and reconciles again.
func (f *fixture) settle() {
	for range 3 {
		f.m.Reconcile()
		for {
			f.m.mu.Lock()
			n := len(f.m.inflight)
			f.m.mu.Unlock()
			if n == 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// R15 (S5): a dataset that was never ready, whose head advances five times while new
// content cannot be fetched, keeps its last fully held view pinned and stages at most two
// heads; later heads are only observed.
func TestFirstHeldProtection(t *testing.T) {
	unavailable := errors.New("no member has it")
	f := newFixture(t, func(context.Context, cid.CID, []wire.PeerID, func(cid.CID) bool, func(cid.CID) []wire.PeerID) error {
		return unavailable
	})
	t0, err := f.st.AddBlob(bytes.NewReader(randBlob(0, 4096)), 4096)
	if err != nil {
		t.Fatal(err)
	}
	f.head(t, t0)
	f.settle()
	s, _ := f.m.Status("ds")
	if !s.Complete || s.State != cluster.SyncHeld || s.Protection.Held != t0.String() || s.Protection.Ready != "" {
		t.Fatalf("first head: %+v", s)
	}
	var heads []cid.CID
	for i := uint64(1); i <= 5; i++ {
		h, _ := cid.SumCID(cid.Raw, cid.BLAKE3, randBlob(i, 100))
		heads = append(heads, h)
		f.head(t, h)
		f.settle()
	}
	s, _ = f.m.Status("ds")
	p := f.m.Protected("ds")
	if p.Held != t0.String() || len(p.Staged) != 2 || p.Staged[0] != heads[0].String() || p.Staged[1] != heads[1].String() {
		t.Fatalf("protection %+v", p)
	}
	if s.Observed != heads[4] || s.Target != heads[1] || s.Reported.Target != heads[1].String() || s.Complete {
		t.Fatalf("status %+v", s)
	}
	want := []string{t0.String(), heads[0].String(), heads[1].String()}
	slices.Sort(want)
	if got := f.pinned(t); !slices.Equal(got, want) {
		t.Fatalf("pins %v, want %v", got, want)
	}
	if !f.has(t0) {
		t.Fatal("first held view lost")
	}
}

// R17 (S7): a full store pauses acquisition with the bytes needed; protected pins and
// content stay.
func TestPressurePauses(t *testing.T) {
	var f *fixture
	big := randBlob(9, 1<<20)
	f = newFixture(t, func(ctx context.Context, root cid.CID, _ []wire.PeerID, _ func(cid.CID) bool, _ func(cid.CID) []wire.PeerID) error {
		_, err := f.st.PutBlob(root, bytes.NewReader(big), int64(len(big)))
		return err
	})
	t0, err := f.st.AddBlob(bytes.NewReader(randBlob(0, 4096)), 4096)
	if err != nil {
		t.Fatal(err)
	}
	f.head(t, t0)
	f.settle()
	stats, _ := f.st.Stats()
	f.st.SetLimits(store.Limits{Physical: stats.Bytes + 64<<10})
	t1, _ := cid.SumCID(cid.Raw, cid.BLAKE3, big)
	f.head(t, t1)
	f.settle()
	s, _ := f.m.Status("ds")
	if s.State != cluster.SyncPaused || s.Reason != "insufficient_storage" || s.Need <= 0 || s.Complete || s.Target != t1 {
		t.Fatalf("status %+v", s)
	}
	if got := f.pinned(t); !slices.Contains(got, t0.String()) || !f.has(t0) {
		t.Fatalf("protected content released: pins %v", got)
	}
	if s.Reported.State != cluster.SyncPaused || s.Reported.Complete {
		t.Fatalf("report %+v", s.Reported)
	}
}

// A placement fetch that fails at once (the owner is unreachable) is retried with a
// bounded exponential backoff instead of re-running the reconcile pass on every failure
// (0076); a cluster state change resets the backoff and retries promptly.
func TestFailedFetchBackoff(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	self := peer(t, 1)
	c, err := cluster.Create(cluster.Options{Dir: filepath.Join(dir, "c"), Store: st}, "lab", cluster.ModePrivate, self, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(
		cluster.Op{Collection: cluster.NSMember, RKey: peer(t, 2).String(), Value: &cluster.Member{Roles: []string{cluster.RoleStorage}, State: cluster.StateActive}},
		cluster.Op{Collection: cluster.NSDataset, RKey: "ds", Value: &cluster.Dataset{Source: cluster.Source{Local: true}, Policy: cluster.Policy{Replicas: 2}}},
	); err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int64
	down := func(context.Context, cid.CID, []wire.PeerID, func(cid.CID) bool, func(cid.CID) []wire.PeerID) error {
		attempts.Add(1)
		return errors.New("owner unreachable")
	}
	m, err := Open(Config{Store: st, Cluster: c, Self: self, Dir: filepath.Join(dir, "c"), Fetch: down, Interval: 10 * time.Millisecond, Backoff: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	m.Start()
	f := &fixture{st: st, c: c, m: m, self: self}
	f.head(t, cid.MustParse("bafkr4ihkr4ld3m4gqkjf4reryxsy2s5tkbxprqkow6fin2iiyvreuzzab4"))
	start := time.Now()
	time.Sleep(300 * time.Millisecond)
	passes, n := m.passes.Load(), attempts.Load()
	t.Logf("%d passes, %d attempts", passes, n)
	// Retries at 20, 40, 80, 160 ms after each failure: about five attempts in 300 ms,
	// and passes bounded by the 10 ms ticker plus one per retry.
	if n < 2 || n > 8 {
		t.Fatalf("%d fetch attempts in %s, want a handful", n, time.Since(start))
	}
	if passes > 60 {
		t.Fatalf("%d reconcile passes in %s with a failing fetch", passes, time.Since(start))
	}
	// A state change resets the backoff: the next attempt follows at once.
	before := attempts.Load()
	if _, err := c.Write(cluster.Op{Collection: cluster.NSMember, RKey: peer(t, 3).String(), Value: &cluster.Member{Roles: []string{cluster.RoleStorage}, State: cluster.StateActive}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for attempts.Load() == before {
		if time.Now().After(deadline) {
			t.Fatal("no retry after a cluster state change")
		}
		time.Sleep(time.Millisecond)
	}
}
