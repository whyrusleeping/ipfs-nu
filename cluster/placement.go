package cluster

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"slices"

	"lukechampine.com/blake3"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// EpochLocal is the placement epoch of the implicit local cluster (E3).
const EpochLocal = "local"

// DefaultWeight is the placement weight of a storage member whose record has no capacity
// (capacity 0): 1 TiB, so unconfigured members share placement evenly with members
// whose capacity is set in bytes of the same order.
const DefaultWeight = 1 << 40

// Eligible is a storage member that receives placement under a policy.
type Eligible struct {
	Peer   wire.PeerID
	Weight float64
	Tags   map[string]string
}

// Placement maps blobs to their owners under one dataset policy in one epoch
// (docs/cluster.md "Placement"). It is a pure function of merged state.
type Placement struct {
	Policy Policy
	Epoch  string
	// Members are the eligible members: storage role, state active, matching Where,
	// sorted by PeerId bytes.
	Members []Eligible
	k       int
}

// Epoch is the placement epoch of v: the hex SHA-256 of "nu.cluster.epoch.v1\0" ‖ the
// DRISL array of {peer (PeerId bytes), capacity, tags, state} of every non-removed member
// with the storage role, sorted by PeerId bytes. Any change of a storage member's
// capacity, tags or state (or membership) changes it; the local cluster's is EpochLocal.
func Epoch(v *View) string {
	if v.Genesis.Mode == ModeLocal {
		return EpochLocal
	}
	type ent struct {
		Peer     []byte            `drisl:"peer"`
		Capacity int64             `drisl:"capacity"`
		Tags     map[string]string `drisl:"tags"`
		State    string            `drisl:"state"`
	}
	var es []ent
	for id, m := range v.Members {
		if m.Deleted || m.Value == nil || m.Value.State == StateRemoved || !slices.Contains(m.Value.Roles, RoleStorage) {
			continue
		}
		tags := m.Value.Tags
		if tags == nil {
			tags = map[string]string{}
		}
		es = append(es, ent{Peer: id.Bytes(), Capacity: m.Value.Capacity, Tags: tags, State: m.Value.State})
	}
	slices.SortFunc(es, func(a, b ent) int { return bytes.Compare(a.Peer, b.Peer) })
	if es == nil {
		es = []ent{}
	}
	b, err := drisl.Marshal(es)
	if err != nil {
		panic(err) // plain values always encode
	}
	h := sha256.New()
	h.Write([]byte("nu.cluster.epoch.v1\x00"))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// NewPlacement computes the eligible members of policy p in v. In the local cluster the
// only eligible member is self (the one-node owner set, E6).
func NewPlacement(v *View, p Policy, self wire.PeerID) *Placement {
	pl := &Placement{Policy: p, Epoch: Epoch(v)}
	if v.Genesis.Mode == ModeLocal {
		pl.Members = []Eligible{{Peer: self, Weight: DefaultWeight}}
	} else {
		for id, m := range v.Members {
			if m.Deleted || m.Value == nil || m.Value.State != StateActive || !slices.Contains(m.Value.Roles, RoleStorage) || !matches(p.Where, m.Value.Tags) {
				continue
			}
			w := float64(m.Value.Capacity)
			if m.Value.Capacity <= 0 {
				w = DefaultWeight
			}
			pl.Members = append(pl.Members, Eligible{Peer: id, Weight: w, Tags: m.Value.Tags})
		}
		slices.SortFunc(pl.Members, func(a, b Eligible) int { return bytes.Compare(a.Peer.Bytes(), b.Peer.Bytes()) })
	}
	pl.k = int(p.Replicas)
	if p.All {
		pl.k = len(pl.Members)
	}
	return pl
}

func matches(where map[string][]string, tags map[string]string) bool {
	for k, vals := range where {
		v, ok := tags[k]
		if !ok || !slices.Contains(vals, v) {
			return false
		}
	}
	return true
}

// K is the number of copies per blob the policy asks for (with All, the eligible count).
func (pl *Placement) K() int { return pl.k }

// Eligible reports whether id is an eligible member.
func (pl *Placement) Eligible(id wire.PeerID) bool {
	for _, m := range pl.Members {
		if m.Peer == id {
			return true
		}
	}
	return false
}

// Feasibility is the E5 check of a policy in an epoch.
type Feasibility struct {
	OK bool
	// MembersMissing and DomainsMissing are the shortfalls; Unmet names them.
	MembersMissing, DomainsMissing int
	Unmet                          []string
}

// Feasible checks E5: at least K eligible active storage members and, with Spread, at
// least K distinct non-empty values of the spread tag among them. Collections with
// replicas 0 have no guaranteed copies and are never "ready" (see Ready).
func (pl *Placement) Feasible() Feasibility {
	var f Feasibility
	k := pl.k
	if pl.Policy.All && k == 0 {
		k = 1
	}
	if k <= 0 {
		f.Unmet = append(f.Unmet, "no_guaranteed_copies")
		return f
	}
	if n := len(pl.Members); n < k {
		f.MembersMissing = k - n
		f.Unmet = append(f.Unmet, fmt.Sprintf("policy_unsatisfiable: %d eligible storage member(s), %d required", n, k))
	}
	if t := pl.Policy.Spread; t != "" {
		seen := map[string]bool{}
		for _, m := range pl.Members {
			if v := m.Tags[t]; v != "" {
				seen[v] = true
			}
		}
		if len(seen) < k {
			f.DomainsMissing = k - len(seen)
			f.Unmet = append(f.Unmet, fmt.Sprintf("policy_unsatisfiable: %d distinct %q value(s), %d required", len(seen), t, k))
		}
	}
	f.OK = len(f.Unmet) == 0
	return f
}

// Score is the weighted rendezvous score of member peer for blob mh:
// u = (uint64be(blake3("nu.place\0" ‖ PeerId bytes ‖ binary multihash)[0:8]) + 1) / (2^64 + 1)
// evaluated in float64 (so the denominator is 2^64), u clamped below 1, and
// score = −weight / ln(u).
func Score(peer wire.PeerID, mh cid.Multihash, weight float64) float64 {
	h := blake3.New(32, nil)
	h.Write([]byte("nu.place\x00"))
	h.Write(peer.Bytes())
	h.Write(mh.Bytes())
	x := binary.BigEndian.Uint64(h.Sum(nil)[:8])
	u := (float64(x) + 1) / (math.Exp2(64) + 1)
	if u >= 1 {
		u = math.Nextafter(1, 0)
	}
	return -weight / math.Log(u)
}

// Ranked returns every eligible member ordered by descending score for mh (ties: PeerId
// bytes ascending). Fetches try sources in this order after the owners.
func (pl *Placement) Ranked(mh cid.Multihash) []wire.PeerID {
	type sc struct {
		i int
		s float64
	}
	ss := make([]sc, len(pl.Members))
	for i, m := range pl.Members {
		ss[i] = sc{i, Score(m.Peer, mh, m.Weight)}
	}
	slices.SortStableFunc(ss, func(a, b sc) int {
		switch {
		case a.s > b.s:
			return -1
		case a.s < b.s:
			return 1
		}
		return a.i - b.i // Members are sorted by PeerId bytes
	})
	out := make([]wire.PeerID, len(ss))
	for i, s := range ss {
		out[i] = pl.Members[s.i].Peer
	}
	return out
}

// Owners returns the owners of mh in rank order: with Spread, first the best-ranked
// member of each distinct spread value (untagged members have none) up to K, then the
// best remaining members up to K. Fewer than K eligible members yield all of them; the
// policy is then infeasible (Feasible).
func (pl *Placement) Owners(mh cid.Multihash) []wire.PeerID {
	k := pl.k
	if k <= 0 {
		return nil
	}
	ranked := pl.Ranked(mh)
	if len(ranked) <= k {
		return ranked
	}
	t := pl.Policy.Spread
	if t == "" {
		return ranked[:k]
	}
	tags := map[wire.PeerID]string{}
	for _, m := range pl.Members {
		tags[m.Peer] = m.Tags[t]
	}
	out := make([]wire.PeerID, 0, k)
	taken := map[wire.PeerID]bool{}
	seen := map[string]bool{}
	for _, id := range ranked {
		if len(out) == k {
			break
		}
		if v := tags[id]; v != "" && !seen[v] {
			seen[v] = true
			taken[id] = true
			out = append(out, id)
		}
	}
	for _, id := range ranked {
		if len(out) == k {
			break
		}
		if !taken[id] {
			out = append(out, id)
		}
	}
	return out
}

// Owns reports whether id is an owner of mh.
func (pl *Placement) Owns(id wire.PeerID, mh cid.Multihash) bool {
	return slices.Contains(pl.Owners(mh), id)
}
