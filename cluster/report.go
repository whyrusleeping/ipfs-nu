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

// ReadyInput is what the E5–E8 evaluation needs; it is a pure function of it.
type ReadyInput struct {
	Target, Mode, Policy, Epoch string
	// Live, when set, evaluates archive-wide readiness (E8): reports must also be bound to
	// this live set and say it is complete.
	Live        string
	Feasibility Feasibility
	// Owners is the owner set of Target in Epoch; OwnersKnown is false when this node
	// cannot compute it (it lacks the target's structure).
	Owners      []wire.PeerID
	OwnersKnown bool
	// Members restricts extra evidence to current members (nil: all).
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

// ReadyResult is the evaluation (E6–E8): State, the counted confirmations, the owners
// still missing, extra fresh complete reports from non-owners (never counted) and the
// age of the oldest counted report.
type ReadyResult struct {
	State       string
	Confirmed   []Confirmation
	Required    int
	Missing     []MissingOwner
	Extra       []Confirmation
	EvidenceAge time.Duration
	Unmet       []string
}

// Ready reports whether the input's evidence makes Target ready (E5–E8).
func Ready(in ReadyInput) ReadyResult {
	fresh := in.Fresh
	if fresh <= 0 {
		fresh = DefaultFresh
	}
	res := ReadyResult{Required: len(in.Owners)}
	if !in.Feasibility.OK {
		res.State, res.Unmet = ReadyUnsatisfied, slices.Clone(in.Feasibility.Unmet)
		return res
	}
	if !in.OwnersKnown {
		res.State = ReadyUnknown
		res.Unmet = []string{"owner set unknown: this node does not hold the target's structure"}
		return res
	}
	owner := map[wire.PeerID]bool{}
	contradicted := false
	for _, id := range in.Owners {
		owner[id] = true
		ev, ok := in.Evidence[id]
		age := in.Now.Sub(ev.Received)
		reason := ""
		switch r := ev.Report; {
		case !ok:
			reason = ReasonAbsent
		case age > fresh:
			reason = ReasonStale
		case r.Target != in.Target || (in.Live != "" && r.Live != in.Live):
			reason = ReasonAbsent
		case r.Epoch != in.Epoch || r.Policy != in.Policy || r.Mode != in.Mode:
			reason = ReasonEpoch
		case !r.Complete || (in.Live != "" && !r.LiveComplete()):
			reason = ReasonIncomplete
		}
		if reason == "" {
			res.Confirmed = append(res.Confirmed, Confirmation{Peer: id, Seq: ev.Report.Seq, Age: max(age, 0)})
			res.EvidenceAge = max(res.EvidenceAge, age)
			continue
		}
		if reason != ReasonStale {
			contradicted = true
		}
		res.Missing = append(res.Missing, MissingOwner{Peer: id, Reason: reason})
		res.Unmet = append(res.Unmet, fmt.Sprintf("owner %s: %s", id, reason))
	}
	for id, ev := range in.Evidence {
		r := ev.Report
		age := in.Now.Sub(ev.Received)
		if owner[id] || age > fresh || !r.Complete || r.Target != in.Target || (in.Members != nil && !in.Members(id)) ||
			(in.Live != "" && (r.Live != in.Live || !r.LiveComplete())) {
			continue
		}
		res.Extra = append(res.Extra, Confirmation{Peer: id, Seq: r.Seq, Age: max(age, 0)})
	}
	sortConf := func(cs []Confirmation) {
		slices.SortFunc(cs, func(a, b Confirmation) int { return cmpPeer(a.Peer, b.Peer) })
	}
	sortConf(res.Confirmed)
	sortConf(res.Extra)
	switch {
	case len(res.Missing) == 0:
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
