package cluster

import (
	"slices"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

// The readiness evaluation (E3–E8) on a fake clock: one row per negative-matrix case
// that does not need a network (docs/operations.md R4–R8), counting confirmed copies per
// unit (k-of-n).
func TestReadyMatrix(t *testing.T) {
	a, b, c, d := fixedPeer(t, 40), fixedPeer(t, 41), fixedPeer(t, 42), fixedPeer(t, 43)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	const T, T2, P, E, E1 = "bafyT", "bafyT2", "pol", "ep2", "ep1"
	rep := func(target, epoch string, complete bool) Report {
		return Report{Name: "ds", Target: target, Mode: ModeOwned, Policy: P, Epoch: epoch, Complete: complete, Seq: 7}
	}
	ev := func(r Report, age time.Duration) Evidence { return Evidence{Report: r, Received: now.Add(-age)} }
	ok := Feasibility{OK: true}
	base := func() ReadyInput {
		return ReadyInput{Target: T, Policy: P, Epoch: E, Feasibility: ok, K: 2, Groups: []OwnerGroup{{Owners: []wire.PeerID{a, b}, Units: 3}}, OwnersKnown: true, Now: now,
			Evidence: map[wire.PeerID]Evidence{a: ev(rep(T, E, true), time.Second), b: ev(rep(T, E, true), 3*time.Second)}}
	}
	reasons := func(r ReadyResult) map[wire.PeerID]string {
		m := map[wire.PeerID]string{}
		for _, x := range r.Missing {
			m[x.Peer] = x.Reason
		}
		return m
	}
	peers := func(cs []Confirmation) []wire.PeerID {
		var out []wire.PeerID
		for _, c := range cs {
			out = append(out, c.Peer)
		}
		slices.Sort(out)
		return out
	}

	t.Run("ready", func(t *testing.T) {
		r := Ready(base())
		if r.State != ReadyReady || r.Required != 2 || len(r.Confirmed) != 2 || r.EvidenceAge != 3*time.Second || r.Copies != (Copies{"blob", 3, 0, 2}) || len(r.Unmet) != 0 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("R4 report for another epoch", func(t *testing.T) {
		in := base()
		in.Evidence[b] = ev(rep(T, E1, true), time.Second)
		r := Ready(in)
		if r.State == ReadyReady || reasons(r)[b] != ReasonEpoch || len(r.Confirmed) != 1 || r.Copies.Below != 3 || r.Copies.Min != 1 {
			t.Fatalf("%+v", r)
		}
		in.Evidence[b] = ev(Report{Name: "ds", Target: T, Mode: ModeOwned, Policy: "other", Epoch: E, Complete: true}, 0)
		if r := Ready(in); reasons(r)[b] != ReasonEpoch {
			t.Fatalf("another policy: %+v", r)
		}
	})
	t.Run("R5 clock jump past evidence.fresh", func(t *testing.T) {
		in := base()
		in.WasReady = true
		in.Now = now.Add(DefaultFresh + time.Second)
		r := Ready(in)
		if r.State != ReadyUnknown || reasons(r)[a] != ReasonStale || reasons(r)[b] != ReasonStale || len(r.Confirmed) != 0 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("E8 degraded on fresh contradiction", func(t *testing.T) {
		in := base()
		in.WasReady = true
		in.Evidence[a] = ev(rep(T, E, false), 0)
		if r := Ready(in); r.State != ReadyDegraded || reasons(r)[a] != ReasonIncomplete {
			t.Fatalf("%+v", r)
		}
		in.Evidence[a] = ev(rep(T2, E, true), 0) // the owner omits T
		if r := Ready(in); r.State != ReadyDegraded || reasons(r)[a] != ReasonAbsent {
			t.Fatalf("%+v", r)
		}
		in.WasReady = false
		if r := Ready(in); r.State != ReadySyncing {
			t.Fatalf("never ready: %+v", r)
		}
	})
	t.Run("R6 infeasible policy", func(t *testing.T) {
		in := base()
		in.Feasibility = Feasibility{MembersMissing: 1, Unmet: []string{"policy_unsatisfiable: 1 eligible storage member(s), 2 required"}}
		if r := Ready(in); r.State != ReadyUnsatisfied || len(r.Unmet) != 1 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("R7 k-of-n: a full holder covers a stale owner", func(t *testing.T) {
		in := base()
		in.Groups = []OwnerGroup{{Owners: []wire.PeerID{a, b}, Units: 2}, {Owners: []wire.PeerID{b, c}, Units: 1}}
		in.Evidence[b] = ev(rep(T, E, true), DefaultFresh+time.Second)
		in.Evidence[c] = ev(rep(T, E, true), 0)
		r := Ready(in)
		if r.State == ReadyReady || r.Copies.Below != 3 || r.Copies.Min != 1 {
			t.Fatalf("without the full holder: %+v", r)
		}
		in.Evidence[d] = ev(Report{Name: "ds", Target: T, Mode: ModeFull, Policy: P, Epoch: E, Complete: true}, 0)
		r = Ready(in)
		if r.State != ReadyReady || !slices.Equal(peers(r.Confirmed), peers([]Confirmation{{Peer: a}, {Peer: c}, {Peer: d}})) ||
			reasons(r)[b] != ReasonStale || len(r.Unmet) != 0 || r.Copies != (Copies{"blob", 3, 0, 2}) {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("incomplete surplus holder", func(t *testing.T) {
		in := base()
		in.Evidence[c] = ev(Report{Name: "ds", Target: T, Mode: ModeFull, Policy: P, Epoch: E}, 0)
		r := Ready(in)
		if r.State != ReadyReady || len(r.Confirmed) != 2 || len(r.Extra) != 0 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("R8 non-owner holdings complete, owners not", func(t *testing.T) {
		in := base()
		in.Evidence[a] = ev(rep(T, E, false), 0)
		delete(in.Evidence, b)
		in.Evidence[c] = ev(rep(T, E, true), 0) // owns no unit: confirms nothing
		r := Ready(in)
		if r.State == ReadyReady || len(r.Extra) != 1 || r.Extra[0].Peer != c || len(r.Confirmed) != 0 || reasons(r)[b] != ReasonAbsent || r.Copies.Min != 0 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("owner set unknown", func(t *testing.T) {
		in := base()
		in.OwnersKnown = false
		if r := Ready(in); r.State != ReadyUnknown {
			t.Fatalf("%+v", r)
		}
	})
}

// Policy hashes bind reports to the signed policy map, so any policy edit unbinds them.
func TestPolicyHash(t *testing.T) {
	h := HLC(1767225600000000).String()
	dec := func(p Policy) string {
		b, err := encodeRecord(&Dataset{Source: Source{Local: true}, Policy: p, HLC: h})
		if err != nil {
			t.Fatal(err)
		}
		r, err := DecodeRecord(NSDataset+"/ds", b)
		if err != nil {
			t.Fatal(err)
		}
		return r.Value.(*Dataset).PolicyHash()
	}
	a, b := dec(Policy{Replicas: 2}), dec(Policy{Replicas: 2, Spread: "zone"})
	if len(a) != 64 || a == b || a != dec(Policy{Replicas: 2}) {
		t.Fatalf("policy hashes %q %q", a, b)
	}
}

// Heartbeat report deltas: a receiver at the delta's base applies it, a receiver that
// missed one pulls, and removals propagate.
func TestReportDelta(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	newSvc := func() *Service {
		return &Service{c: founder(t, clk), own: map[string]Report{}, evidence: make(chan struct{}), peers: map[wire.PeerID]*PeerStatus{}}
	}
	snd, rcv := newSvc(), newSvc()
	p := &PeerStatus{}
	r1 := Report{Name: "a", Target: "T1", Mode: ModeOwned, State: SyncSyncing}
	r2 := Report{Name: "b", Target: "T2", Mode: ModeOwned, State: SyncHeld, Complete: true}
	d := snd.reportDelta([]Report{r1, r2})
	if d.Base != 0 || len(d.List) != 2 || rcv.applyReportsLocked(p, d, clk.t) || len(p.Reports) != 2 {
		t.Fatalf("first delta %+v → %+v", d, p)
	}
	// Unchanged: an empty delta re-confirms everything.
	clk.t = clk.t.Add(time.Minute)
	d = snd.reportDelta([]Report{r1, r2})
	if len(d.List) != 0 || rcv.applyReportsLocked(p, d, clk.t) || !p.ReportsAt.Equal(clk.t) {
		t.Fatalf("empty delta %+v → %+v", d, p)
	}
	// One change carries one report with a newer seq.
	r1.Complete, r1.State = true, SyncHeld
	d = snd.reportDelta([]Report{r1, r2})
	if len(d.List) != 1 || d.List[0].Name != "a" || rcv.applyReportsLocked(p, d, clk.t) || !p.Reports["a"].Complete {
		t.Fatalf("change %+v → %+v", d, p)
	}
	// A missed heartbeat leaves a gap: the receiver must pull.
	_ = snd.reportDelta([]Report{r2})
	d = snd.reportDelta([]Report{r2})
	if !rcv.applyReportsLocked(p, d, clk.t) {
		t.Fatal("gap not detected")
	}
	set := snd.ownSet()
	if len(set.Reports) != 1 || set.Reports[0].Name != "b" || set.Seq != d.Seq {
		t.Fatalf("full set %+v", set)
	}
	// A removal in sequence is applied.
	q := &PeerStatus{}
	s2 := newSvc()
	rcv.applyReportsLocked(q, s2.reportDelta([]Report{r1, r2}), clk.t)
	d = s2.reportDelta([]Report{r2})
	if rcv.applyReportsLocked(q, d, clk.t) || len(q.Reports) != 1 || len(d.Gone) != 1 {
		t.Fatalf("removal %+v → %+v", d, q)
	}
}
