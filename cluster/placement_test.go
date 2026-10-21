package cluster

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/p2p/wire"
)

const placementVectorFile = "../testdata/vectors/nu-cluster/placement.json"

type placementVectors struct {
	Members []struct {
		Peer     string            `json:"peer"`
		Capacity int64             `json:"capacity"`
		Tags     map[string]string `json:"tags,omitempty"`
		State    string            `json:"state"`
	} `json:"members"`
	Epoch string `json:"epoch"`
	Blobs []struct {
		Multihash string   `json:"multihash"`
		Scores    []string `json:"scores"` // per member, %.17g
		Ranked    []string `json:"ranked"`
		Owners2   []string `json:"owners_k2"`
		Spread2   []string `json:"owners_k2_spread_zone"`
	} `json:"blobs"`
}

// testView builds a networked view with the given members.
func testView(t *testing.T, members map[wire.PeerID]*Member) *View {
	v := &View{Genesis: Genesis{Name: "lab", Mode: ModePrivate}, Members: map[wire.PeerID]Signed[Member]{}}
	for id, m := range members {
		v.Members[id] = Signed[Member]{Value: m}
	}
	return v
}

func buildPlacementVectors(t *testing.T) placementVectors {
	var out placementVectors
	ms := map[wire.PeerID]*Member{}
	specs := []struct {
		cap   int64
		zone  string
		state string
	}{{0, "eu-1", StateActive}, {4 << 40, "eu-1", StateActive}, {1 << 40, "eu-2", StateActive}, {2 << 40, "", StateActive}, {1 << 40, "eu-3", StateDraining}}
	var peers []wire.PeerID
	for i, s := range specs {
		id := fixedPeer(t, byte(10+i))
		peers = append(peers, id)
		m := &Member{Roles: []string{RoleStorage}, Capacity: s.cap, State: s.state}
		if s.zone != "" {
			m.Tags = map[string]string{"zone": s.zone}
		}
		ms[id] = m
		out.Members = append(out.Members, struct {
			Peer     string            `json:"peer"`
			Capacity int64             `json:"capacity"`
			Tags     map[string]string `json:"tags,omitempty"`
			State    string            `json:"state"`
		}{id.String(), s.cap, m.Tags, s.state})
	}
	v := testView(t, ms)
	out.Epoch = Epoch(v)
	pl := NewPlacement(v, Policy{Replicas: 2}, noSelf)
	sp := NewPlacement(v, Policy{Replicas: 2, Spread: "zone"}, noSelf)
	str := func(ids []wire.PeerID) []string {
		s := []string{}
		for _, id := range ids {
			s = append(s, id.String())
		}
		return s
	}
	for i := range 6 {
		mh := cid.Sum(cid.BLAKE3, fmt.Appendf(nil, "blob %d", i))
		if i == 5 {
			mh = cid.Sum(cid.SHA256, []byte("structure"))
		}
		var scores []string
		for _, m := range pl.Members {
			scores = append(scores, fmt.Sprintf("%.17g", Score(m.Peer, mh, m.Weight)))
		}
		out.Blobs = append(out.Blobs, struct {
			Multihash string   `json:"multihash"`
			Scores    []string `json:"scores"`
			Ranked    []string `json:"ranked"`
			Owners2   []string `json:"owners_k2"`
			Spread2   []string `json:"owners_k2_spread_zone"`
		}{mh.String(), scores, str(pl.Ranked(mh)), str(pl.Owners(mh)), str(sp.Owners(mh))})
	}
	_ = peers
	return out
}

// Placement (scores, ranking, owners with and without spread, epoch) is a cross-node
// contract: every member must compute the same owners.
func TestPlacementVectors(t *testing.T) {
	got := buildPlacementVectors(t)
	if os.Getenv("NU_UPDATE_VECTORS") == "1" {
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(placementVectorFile, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(placementVectorFile)
	if err != nil {
		t.Fatal(err)
	}
	var want placementVectors
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("placement vectors changed:\n got %+v\nwant %+v", got, want)
	}
}

func TestPlacementRules(t *testing.T) {
	a, b, c, d := fixedPeer(t, 30), fixedPeer(t, 31), fixedPeer(t, 32), fixedPeer(t, 33)
	storage := func(zone, state string) *Member {
		m := &Member{Roles: []string{RoleStorage}, State: state}
		if zone != "" {
			m.Tags = map[string]string{"zone": zone}
		}
		return m
	}
	v := testView(t, map[wire.PeerID]*Member{a: storage("z1", StateActive), b: storage("z1", StateActive), c: storage("z2", StateActive),
		d: storage("z3", StateDraining)})
	v.Members[fixedPeer(t, 34)] = Signed[Member]{Value: &Member{Roles: []string{RoleClient}, State: StateActive}}

	// Draining, unavailable and client members own nothing.
	pl := NewPlacement(v, Policy{Replicas: 2}, noSelf)
	if len(pl.Members) != 3 || pl.Eligible(d) {
		t.Fatalf("eligible %v", pl.Members)
	}
	// E5: k=4 on three members, spread over two zones for k=3.
	if f := NewPlacement(v, Policy{Replicas: 4}, noSelf).Feasible(); f.OK || f.MembersMissing != 1 {
		t.Fatalf("k=4 feasibility %+v", f)
	}
	if f := NewPlacement(v, Policy{Replicas: 3, Spread: "zone"}, noSelf).Feasible(); f.OK || f.DomainsMissing != 1 || f.MembersMissing != 0 {
		t.Fatalf("k=3 spread feasibility %+v", f)
	}
	if f := NewPlacement(v, Policy{Replicas: 2, Spread: "zone"}, noSelf).Feasible(); !f.OK {
		t.Fatalf("k=2 spread feasibility %+v", f)
	}
	// Where narrows the eligible set; All takes every eligible member.
	w := NewPlacement(v, Policy{All: true, Where: map[string][]string{"zone": {"z1"}}}, noSelf)
	if w.K() != 2 || w.Eligible(c) {
		t.Fatalf("where/all: k=%d members %v", w.K(), w.Members)
	}
	// With spread, the two owners are always in distinct zones.
	sp := NewPlacement(v, Policy{Replicas: 2, Spread: "zone"}, noSelf)
	for i := range 64 {
		mh := cid.Sum(cid.BLAKE3, fmt.Appendf(nil, "%d", i))
		o := sp.Owners(mh)
		if len(o) != 2 || (o[0] != c && o[1] != c) {
			t.Fatalf("spread owners %v", o)
		}
		if r := pl.Ranked(mh); !reflect.DeepEqual(pl.Owners(mh), r[:2]) {
			t.Fatal("owners are not the top of the ranking")
		}
	}
	// Adding a member moves roughly its share, not everything.
	e := fixedPeer(t, 35)
	v2 := testView(t, map[wire.PeerID]*Member{a: storage("z1", StateActive), b: storage("z1", StateActive), c: storage("z2", StateActive),
		e: storage("z2", StateActive)})
	pl2 := NewPlacement(v2, Policy{Replicas: 1}, noSelf)
	pl1 := NewPlacement(v, Policy{Replicas: 1}, noSelf)
	moved := 0
	for i := range 1000 {
		mh := cid.Sum(cid.BLAKE3, fmt.Appendf(nil, "%d", i))
		if pl1.Owners(mh)[0] != pl2.Owners(mh)[0] {
			moved++
			if pl2.Owners(mh)[0] != e {
				t.Fatal("a blob moved between existing members")
			}
		}
	}
	if moved < 150 || moved > 350 {
		t.Fatalf("%d of 1000 blobs moved to the fourth member", moved)
	}
	if Epoch(v) == Epoch(v2) || Epoch(&View{Genesis: Genesis{Mode: ModeLocal}}) != EpochLocal {
		t.Fatal("epoch does not follow membership")
	}
}

var noSelf wire.PeerID
