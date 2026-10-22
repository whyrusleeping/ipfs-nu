package cluster

import (
	"fmt"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

// Safe release (docs/operations.md S1–S4): a member releases a blob it holds only when
// the owners of that blob in the current epoch, other than itself, confirm it k times
// with fresh reports bound to the current head, live set, policy and epoch, and only
// once that binding has been settled. This file is the pure decision; package replica
// applies it.

// Release defaults.
const (
	DefaultSettle      = 10 * time.Minute // release.settle
	DefaultRemoveGrace = 24 * time.Hour   // dataset.remove_grace
)

// ReleaseInput is what a release decision for one dataset needs.
type ReleaseInput struct {
	// Target, Live, Policy and Epoch are the binding in this node's merged state.
	Target, Live, Policy, Epoch string
	K                           int
	Self                        wire.PeerID
	// Evidence holds other members' reports for the dataset (this node's own is never
	// evidence for its own release).
	Evidence map[wire.PeerID]Evidence
	Now      time.Time
	Fresh    time.Duration // 0: DefaultFresh
	Settle   time.Duration // 0: DefaultSettle
	// BoundSince is when this node's merged state first showed the binding.
	BoundSince time.Time
	// HeldBack counts records held back for clock skew (S3).
	HeldBack int
	// Members reports current membership; MemberSince when a member's record state was
	// last seen to change (S3: an owner that changed within Settle does not count).
	Members     func(wire.PeerID) bool
	MemberSince func(wire.PeerID) time.Time
	// Displaced lists per-signer heads that disagree with Target and were written
	// within Settle (S4).
	Displaced []string
}

func (in *ReleaseInput) fresh() time.Duration {
	if in.Fresh > 0 {
		return in.Fresh
	}
	return DefaultFresh
}

func (in *ReleaseInput) settle() time.Duration {
	if in.Settle > 0 {
		return in.Settle
	}
	return DefaultSettle
}

// Deferred lists the dataset-wide reasons (S3, S4) that defer every release now; empty
// means the binding is settled.
func (in *ReleaseInput) Deferred() []string {
	var out []string
	if d := in.Now.Sub(in.BoundSince); in.BoundSince.IsZero() || d < in.settle() {
		out = append(out, fmt.Sprintf("unsettled: the current head, policy and epoch have held for %s of release.settle %s", max(d, 0).Round(time.Second), in.settle()))
	}
	if in.HeldBack > 0 {
		out = append(out, fmt.Sprintf("held_back: %d record(s) held back for clock skew", in.HeldBack))
	}
	for id, ev := range in.Evidence {
		r := ev.Report
		if in.Now.Sub(ev.Received) > in.fresh() || (in.Members != nil && !in.Members(id)) {
			continue
		}
		if r.Epoch != in.Epoch || r.Policy != in.Policy {
			out = append(out, fmt.Sprintf("epoch_disagreement: %s reports epoch %.12s policy %.12s (here %.12s, %.12s)", id, r.Epoch, r.Policy, in.Epoch, in.Policy))
		}
	}
	for _, t := range in.Displaced {
		out = append(out, "concurrent_heads: displaced head "+t+" is protected until it settles")
	}
	return out
}

// Confirm counts the owners that confirm a unit (S2): fresh reports, bound to the
// binding, complete (for the live set too), from current members whose record state did
// not change within Settle. The releasing member never counts unless countSelf (when
// what is released is not this unit, such as an old version's root once the head is
// confirmed). It returns the confirming owners and the others with a reason.
func (in *ReleaseInput) Confirm(owners []wire.PeerID, countSelf bool) (confirmed []wire.PeerID, missing []MissingOwner) {
	for _, o := range owners {
		if o == in.Self && !countSelf {
			continue
		}
		ev, ok := in.Evidence[o]
		r := ev.Report
		reason := ""
		switch {
		case in.Members != nil && !in.Members(o):
			reason = ReasonAbsent
		case in.MemberSince != nil && in.Now.Sub(in.MemberSince(o)) < in.settle():
			reason = "member_changed"
		case !ok:
			reason = ReasonAbsent
		case in.Now.Sub(ev.Received) > in.fresh():
			reason = ReasonStale
		case r.Target != in.Target || (in.Live != "" && r.Live != in.Live):
			reason = ReasonAbsent
		case r.Epoch != in.Epoch || r.Policy != in.Policy || (r.Mode != ModeOwned && r.Mode != ModeFull):
			reason = ReasonEpoch
		case !r.Complete || (in.Live != "" && !r.LiveComplete()):
			reason = ReasonIncomplete
		}
		if reason != "" {
			missing = append(missing, MissingOwner{Peer: o, Reason: reason})
			continue
		}
		confirmed = append(confirmed, o)
	}
	return confirmed, missing
}
