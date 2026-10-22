package cluster

import (
	"fmt"
	"slices"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

// Completion-report modes and member sync states (docs/cluster.md "Status", E2).
const (
	ModeOwned = "owned"
	ModeFull  = "full"

	SyncSyncing = "syncing"
	SyncHeld    = "held"
	SyncPaused  = "paused"
	SyncError   = "error"
)

// DefaultFresh is evidence.fresh (E4): how long a report counts after it was received.
const DefaultFresh = 5 * time.Minute

// Counts is a number of blobs and their bytes.
type Counts struct {
	Blobs int64 `drisl:"blobs" json:"blobs"`
	Bytes int64 `drisl:"bytes" json:"bytes"`
}

// Report is a member's completion report for one dataset (E2): the exact target it works
// toward, the mode, policy hash and placement epoch it is bound to, and whether its
// completeness marks show the structure and every blob it is responsible for present.
type Report struct {
	Name     string  `drisl:"name"`
	Target   string  `drisl:"target"`
	Live     string  `drisl:"live,omitempty"`
	Mode     string  `drisl:"mode"`
	Policy   string  `drisl:"policy"`
	Epoch    string  `drisl:"epoch"`
	Complete bool    `drisl:"complete"`
	Missing  *Counts `drisl:"missing,omitempty"`
	Owned    Counts  `drisl:"owned"`
	State    string  `drisl:"state"`
	Seq      int64   `drisl:"seq"`
	// Incomplete lists the live versions (TIDs) whose structure or owned blobs the member
	// lacks, for a dataset with history (at most MaxIncomplete).
	Incomplete []string `drisl:"incomplete,omitempty"`
}

// MaxIncomplete bounds Report.Incomplete.
const MaxIncomplete = 256

// LiveComplete reports whether the report says the live set is complete.
func (r Report) LiveComplete() bool { return r.Complete && len(r.Incomplete) == 0 }

// Same reports whether two reports say the same thing (ignoring seq).
func (r Report) Same(o Report) bool { return r.same(o) }

// same reports whether two reports say the same thing (ignoring seq).
func (r Report) same(o Report) bool {
	if (r.Missing == nil) != (o.Missing == nil) || (r.Missing != nil && *r.Missing != *o.Missing) {
		return false
	}
	return slices.Equal(r.Incomplete, o.Incomplete) && r.Name == o.Name && r.Target == o.Target && r.Live == o.Live &&
		r.Mode == o.Mode && r.Policy == o.Policy && r.Epoch == o.Epoch && r.Complete == o.Complete && r.Owned == o.Owned && r.State == o.State
}

// ReportDelta is the reports part of a heartbeat: the reports whose seq is above Base,
// the datasets reported at Base but no longer, and the sender's report sequence Seq. A
// receiver holding the sender's set at Base applies it; Base 0 replaces the whole set; a
// receiver at another position, or Base -1 (the delta did not fit), pulls the full set
// with the `reports` op. Every applied heartbeat re-confirms all of the sender's reports.
type ReportDelta struct {
	Base int64    `drisl:"base"`
	Seq  int64    `drisl:"seq"`
	List []Report `drisl:"list,omitempty"`
	Gone []string `drisl:"gone,omitempty"`
}

// ReportSet is the answer to a `reports` request: the member's full current set.
type ReportSet struct {
	Seq     int64    `drisl:"seq"`
	Reports []Report `drisl:"reports"`
}

// Evidence is a report with the time this node received (or re-confirmed) it over an
// authenticated connection, on this node's clock.
type Evidence struct {
	Report   Report
	Received time.Time
}

// Missing reasons (E7).
const (
	ReasonStale      = "stale"
	ReasonIncomplete = "incomplete"
	ReasonEpoch      = "epoch"
	ReasonAbsent     = "absent"
)

// Readiness states (docs/operations.md "Readiness and status").
const (
	ReadyReady       = "ready"
	ReadySyncing     = "syncing"
	ReadyUnknown     = "unknown"
	ReadyDegraded    = "degraded"
	ReadyUnsatisfied = "policy_unsatisfiable"
)

// OwnerGroup is a number of placement units (blobs; for collections, items) that have
// the same owners.
type OwnerGroup struct {
	Owners []wire.PeerID
	Units  int64
}

// GroupSet accumulates owner groups.
type GroupSet struct{ m map[string]*OwnerGroup }

// Add counts n units owned by owners.
func (g *GroupSet) Add(owners []wire.PeerID, n int64) {
	if g.m == nil {
		g.m = map[string]*OwnerGroup{}
	}
	o := slices.Clone(owners)
	slices.Sort(o)
	key := ""
	for _, id := range o {
		key += string(id) + "\x00"
	}
	if e := g.m[key]; e != nil {
		e.Units += n
		return
	}
	g.m[key] = &OwnerGroup{Owners: o, Units: n}
}

// List returns the groups in a deterministic order.
func (g *GroupSet) List() []OwnerGroup {
	out := make([]OwnerGroup, 0, len(g.m))
	for _, e := range g.m {
		out = append(out, *e)
	}
	slices.SortFunc(out, func(a, b OwnerGroup) int { return slices.CompareFunc(a.Owners, b.Owners, cmpPeer) })
	return out
}

// ReadyInput is what the E5–E8 evaluation needs; it is a pure function of it.
type ReadyInput struct {
	Target, Policy, Epoch string
	// Live, when set, evaluates archive-wide readiness (E8): reports must also be bound to
	// this live set and say it is complete.
	Live        string
	Feasibility Feasibility
	// K is the number of confirmed copies every unit needs (replicas; the eligible count
	// for replicas: all). Groups are Target's units by owner set in Epoch; OwnersKnown is
	// false when this node cannot compute them (it lacks the target's structure). Unit
	// names the unit ("blob", or "item" for collections).
	K           int
	Groups      []OwnerGroup
	OwnersKnown bool
	Unit        string
	// Members restricts evidence to current members (nil: all).
	Members  func(wire.PeerID) bool
	Evidence map[wire.PeerID]Evidence
	Now      time.Time
	Fresh    time.Duration // 0: DefaultFresh
	// WasReady says Target was ready before (E8: unknown vs degraded).
	WasReady bool
}

// Confirmation is a counted fresh complete report.
type Confirmation struct {
	Peer wire.PeerID
	Seq  int64
	Age  time.Duration
}

// MissingOwner is an owner without a counted report, with the reason.
type MissingOwner struct {
	Peer   wire.PeerID
	Reason string
}

// Copies summarises confirmed copies per unit (E7): Units counted, Below of them with
// fewer than K confirmed copies, and Min, the fewest confirmed copies of any unit.
type Copies struct {
	Unit  string
	Units int64
	Below int64
	Min   int
}

// ReadyResult is the evaluation (E6–E8): State; the counted confirmations; Required, the
// copies each unit needs; the owners without a counted report; extra fresh complete
// reports that confirm no unit; the per-unit copy summary; and the age of the oldest
// counted report.
type ReadyResult struct {
	State       string
	Confirmed   []Confirmation
	Required    int
	Missing     []MissingOwner
	Extra       []Confirmation
	Copies      Copies
	EvidenceAge time.Duration
	Unmet       []string
}

// reportReason classifies one member's evidence for the input's binding ("" counts).
func (in *ReadyInput) reportReason(ev Evidence, ok bool, fresh time.Duration) string {
	r := ev.Report
	switch {
	case !ok:
		return ReasonAbsent
	case in.Now.Sub(ev.Received) > fresh:
		return ReasonStale
	case r.Target != in.Target || (in.Live != "" && r.Live != in.Live):
		return ReasonAbsent
	case r.Epoch != in.Epoch || r.Policy != in.Policy || (r.Mode != ModeOwned && r.Mode != ModeFull):
		return ReasonEpoch
	case !r.Complete || (in.Live != "" && !r.LiveComplete()):
		return ReasonIncomplete
	}
	return ""
}

// Ready evaluates E5–E8 by confirmed copies per unit: a counted report (fresh, bound to
// the target, live set, policy and epoch, complete) in mode owned confirms the units its
// sender owns, in mode full every unit. Target is ready when every unit has at least K
// confirmed copies.
func Ready(in ReadyInput) ReadyResult {
	fresh := in.Fresh
	if fresh <= 0 {
		fresh = DefaultFresh
	}
	res := ReadyResult{Required: in.K, Copies: Copies{Unit: in.Unit}}
	if res.Copies.Unit == "" {
		res.Copies.Unit = "blob"
	}
	if !in.Feasibility.OK {
		res.State, res.Unmet = ReadyUnsatisfied, slices.Clone(in.Feasibility.Unmet)
		return res
	}
	if !in.OwnersKnown {
		res.State = ReadyUnknown
		res.Unmet = []string{"owner set unknown: this node does not hold the target's structure"}
		return res
	}
	counted := map[wire.PeerID]bool{}
	var full []wire.PeerID
	for id, ev := range in.Evidence {
		if (in.Members != nil && !in.Members(id)) || in.reportReason(ev, true, fresh) != "" {
			continue
		}
		counted[id] = true
		if ev.Report.Mode == ModeFull {
			full = append(full, id)
		}
	}
	owner, short := map[wire.PeerID]bool{}, map[wire.PeerID]bool{}
	res.Copies.Min = -1
	for _, g := range in.Groups {
		n := 0
		for _, o := range g.Owners {
			owner[o] = true
			if counted[o] {
				n++
			}
		}
		for _, f := range full {
			if !slices.Contains(g.Owners, f) {
				n++
			}
		}
		res.Copies.Units += g.Units
		if res.Copies.Min < 0 || n < res.Copies.Min {
			res.Copies.Min = n
		}
		if n < in.K {
			res.Copies.Below += g.Units
			for _, o := range g.Owners {
				short[o] = true
			}
		}
	}
	res.Copies.Min = max(res.Copies.Min, 0)
	conf := func(id wire.PeerID) Confirmation {
		ev := in.Evidence[id]
		age := max(in.Now.Sub(ev.Received), 0)
		return Confirmation{Peer: id, Seq: ev.Report.Seq, Age: age}
	}
	for id := range counted {
		if owner[id] || slices.Contains(full, id) {
			c := conf(id)
			res.Confirmed = append(res.Confirmed, c)
			res.EvidenceAge = max(res.EvidenceAge, c.Age)
		} else {
			res.Extra = append(res.Extra, conf(id))
		}
	}
	contradicted := false
	owners := make([]wire.PeerID, 0, len(owner))
	for id := range owner {
		owners = append(owners, id)
	}
	slices.SortFunc(owners, cmpPeer)
	for _, id := range owners {
		if counted[id] {
			continue
		}
		ev, ok := in.Evidence[id]
		reason := in.reportReason(ev, ok, fresh)
		if reason == "" { // fresh and complete, but not a current member
			reason = ReasonAbsent
		}
		if reason != ReasonStale && short[id] {
			contradicted = true
		}
		res.Missing = append(res.Missing, MissingOwner{Peer: id, Reason: reason})
		if short[id] {
			res.Unmet = append(res.Unmet, fmt.Sprintf("owner %s: %s", id, reason))
		}
	}
	sortConf := func(cs []Confirmation) {
		slices.SortFunc(cs, func(a, b Confirmation) int { return cmpPeer(a.Peer, b.Peer) })
	}
	sortConf(res.Confirmed)
	sortConf(res.Extra)
	if res.Copies.Below > 0 {
		res.Unmet = append(res.Unmet, fmt.Sprintf("%d of %d %ss below %d confirmed copies (fewest: %d)",
			res.Copies.Below, res.Copies.Units, res.Copies.Unit, in.K, res.Copies.Min))
	}
	switch {
	case res.Copies.Below == 0 && len(in.Groups) > 0:
		res.State = ReadyReady
	case in.WasReady && contradicted:
		res.State = ReadyDegraded
	case !contradicted:
		res.State = ReadyUnknown
	default:
		res.State = ReadySyncing
	}
	return res
}

func cmpPeer(a, b wire.PeerID) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
