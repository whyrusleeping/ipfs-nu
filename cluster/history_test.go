package cluster

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// Version records: immutable identity vs editable annotations, collisions, compaction,
// and writer scope (docs/cluster.md "Version tree").
func TestVersionMerge(t *testing.T) {
	clk := &fakeClock{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	a := founder(t, clk)
	b := joiner(t, a, clk)
	adminB := newKey(t, b)
	root := a.Genesis().RootKey
	if _, err := a.WriteAs(root, Op{Collection: NSKey, RKey: adminB, Value: &Key{Role: RoleAdmin}},
		Op{Collection: NSDataset, RKey: "photos", Value: &Dataset{Source: Source{Local: true}, Policy: Policy{Replicas: 1, History: ArchiveHistory()}}}); err != nil {
		t.Fatal(err)
	}
	pull(t, a, b)
	const t1, t2 = "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku", "bafkreibm6jg3ux5qumhcn2b3flc3tyu6dmlb4xa7u5bf44yegnrjhc4yeq"
	tid := a.NextTID()
	ver := &Version{Root: t1, Kind: "file", Author: root, Files: 1, Bytes: 5, NewBytes: 5}
	if _, err := a.WriteAs(root, Op{Collection: NSVersion, RKey: VersionKey("photos", tid), Value: ver}); err != nil {
		t.Fatal(err)
	}
	pull(t, a, b)
	h := b.View().VersionHistory("photos")
	if len(h) != 1 || h[0].Root != t1 || h[0].Signer != root {
		t.Fatalf("history = %+v", h)
	}
	// b annotates a's version: a copy of the identity with a label (later hlc wins).
	clk.t = clk.t.Add(time.Second)
	if _, err := b.WriteAs(adminB, Op{Collection: NSVersion, RKey: VersionKey("photos", tid), Value: h[0].Annotated("release-1", "first")}); err != nil {
		t.Fatal(err)
	}
	// b also tries to redefine the version's root under the same TID: a conflict.
	bad := &Version{Root: t2, Kind: "file", Author: adminB, Label: "evil"}
	if _, err := b.WriteAs(adminB, Op{Collection: NSVersion, RKey: VersionKey("photos", tid), Value: bad}); err != nil {
		t.Fatal(err)
	}
	pull(t, b, a)
	h = a.View().VersionHistory("photos")
	if len(h) != 1 || h[0].Root != t1 || h[0].Label != "" || len(h[0].Conflicts) != 1 || h[0].Conflicts[0].Root != t2 {
		t.Fatalf("redefinition not reported as a conflict: %+v", h)
	}
	// Replace b's record with an honest annotation: the label merges.
	if _, err := b.WriteAs(adminB, Op{Collection: NSVersion, RKey: VersionKey("photos", tid), Value: h[0].Annotated("release-1", "first")}); err != nil {
		t.Fatal(err)
	}
	pull(t, b, a)
	h = a.View().VersionHistory("photos")
	if len(h) != 1 || h[0].Label != "release-1" || h[0].Note != "first" || h[0].AnnotatedBy != adminB || len(h[0].Conflicts) != 0 || h[0].Root != t1 {
		t.Fatalf("annotation = %+v", h)
	}
	if v, err := SelectVersion(h, "release-1"); err != nil || v.TID != tid {
		t.Fatalf("label lookup: %v %v", v.TID, err)
	}
	// A winning tombstone compacts the metadata.
	if _, err := a.WriteAs(root, Op{Collection: NSVersion, RKey: VersionKey("photos", tid), Delete: true}); err != nil {
		t.Fatal(err)
	}
	if h := a.View().VersionHistory("photos"); len(h) != 0 {
		t.Fatalf("compacted version still listed: %+v", h)
	}
	if ValidLabel(tid) || ValidLabel("2025-01-31") || !ValidLabel("release-3") {
		t.Fatal("label validation")
	}
}

func entries(times ...time.Time) []VersionEntry {
	var out []VersionEntry
	for i, tm := range times {
		tid := HLC(tm.UnixMicro()).String()
		out = append(out, VersionEntry{TID: tid, Time: tm, Version: Version{Root: "r" + string(rune('a'+i)), NewBytes: 10}})
	}
	return out
}

// Retention: GFS buckets back from the newest version, labels and head always live,
// max_bytes drops the oldest unprotected versions; lookup never falls back.
func TestRetainAndSelect(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	var times []time.Time
	for i := 9; i >= 0; i-- { // ten versions, one per day, newest last
		times = append(times, t0.Add(-time.Duration(i)*24*time.Hour))
	}
	vs := entries(times...)
	vs[0].Label = "first"
	r := Retain("ds", vs, &History{KeepLast: 2, Daily: 3}, vs[9].TID)
	var live []int
	for i, v := range vs {
		if _, ok := r.Live[v.TID]; ok {
			live = append(live, i)
		}
	}
	if !slices.Equal(live, []int{0, 7, 8, 9}) {
		t.Fatalf("live = %v (%v)", live, r.Live)
	}
	if !slices.Contains(r.Live[vs[0].TID], "label") || !slices.Contains(r.Live[vs[9].TID], "head") || r.ID == "" {
		t.Fatalf("reasons %v", r.Live)
	}
	// Nothing new: the same set (nothing pruned while no version arrives).
	if r2 := Retain("ds", vs, &History{KeepLast: 2, Daily: 3}, vs[9].TID); r2.ID != r.ID {
		t.Fatal("live set not deterministic")
	}
	// max_bytes: 20 bytes keeps the protected label and head only; excess reported.
	r = Retain("ds", vs, &History{KeepLast: 5, MaxBytes: 15}, vs[9].TID)
	if len(r.Live) != 2 || r.Excess != 5 {
		t.Fatalf("max_bytes live = %v excess %d", r.Live, r.Excess)
	}
	// Time lookup: newest at or before; before the first version is an error, not a fallback.
	if v, err := SelectVersion(vs, t0.Add(-36*time.Hour).Format(time.RFC3339)); err != nil || v.TID != vs[7].TID {
		t.Fatalf("time lookup = %v, %v", v.TID, err)
	}
	if _, err := SelectVersion(vs, "2020-01-01"); !errors.Is(err, ErrNoVersion) {
		t.Fatalf("time before history: %v", err)
	}
	vs[3].Label = "first"
	if _, err := SelectVersion(vs, "first"); !errors.Is(err, ErrAmbiguousVersion) {
		t.Fatalf("duplicate label: %v", err)
	}
}
