package cluster

import (
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/wire"
)

// The release decision (S2–S4) as a pure function: R11 (a stale owner blocks release
// and is named), R13 (owners in different epochs defer), settle and member changes.
func TestReleaseRules(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	self, x, y := wire.PeerID("self"), wire.PeerID("x"), wire.PeerID("y")
	rep := func(epoch string, complete bool) Report {
		return Report{Target: "T", Policy: "P", Epoch: epoch, Mode: ModeOwned, Complete: complete}
	}
	base := func() ReleaseInput {
		return ReleaseInput{Target: "T", Policy: "P", Epoch: "E", K: 2, Self: self, Now: now, BoundSince: now.Add(-time.Hour),
			Evidence: map[wire.PeerID]Evidence{
				x:    {Report: rep("E", true), Received: now.Add(-time.Second)},
				y:    {Report: rep("E", true), Received: now.Add(-time.Second)},
				self: {Report: rep("E", true), Received: now},
			}}
	}
	in := base()
	if d := in.Deferred(); len(d) != 0 {
		t.Fatalf("settled binding deferred: %v", d)
	}
	if conf, _ := in.Confirm([]wire.PeerID{x, y}, false); len(conf) != 2 {
		t.Fatalf("both owners confirm: %v", conf)
	}
	// The releasing member's own copy never counts for its own release.
	if conf, miss := in.Confirm([]wire.PeerID{self, x}, false); len(conf) != 1 || len(miss) != 0 {
		t.Fatalf("self counted: %v %v", conf, miss)
	}
	// R11: one owner stale.
	in.Evidence[y] = Evidence{Report: rep("E", true), Received: now.Add(-DefaultFresh - time.Second)}
	conf, miss := in.Confirm([]wire.PeerID{x, y}, false)
	if len(conf) != 1 || len(miss) != 1 || miss[0].Peer != y || miss[0].Reason != ReasonStale {
		t.Fatalf("R11: %v %v", conf, miss)
	}
	// Incomplete and absent owners.
	in = base()
	in.Evidence[y] = Evidence{Report: rep("E", false), Received: now}
	delete(in.Evidence, x)
	if conf, miss := in.Confirm([]wire.PeerID{x, y}, false); len(conf) != 0 || miss[0].Reason != ReasonAbsent || miss[1].Reason != ReasonIncomplete {
		t.Fatalf("absent/incomplete: %v %v", conf, miss)
	}
	// R13: an owner reports another epoch: every release defers, and it does not count.
	in = base()
	in.Evidence[y] = Evidence{Report: rep("E2", true), Received: now}
	if d := in.Deferred(); len(d) != 1 || !strings.HasPrefix(d[0], "epoch_disagreement") {
		t.Fatalf("R13 deferred: %v", d)
	}
	if conf, miss := in.Confirm([]wire.PeerID{x, y}, false); len(conf) != 1 || miss[0].Reason != ReasonEpoch {
		t.Fatalf("R13 confirm: %v %v", conf, miss)
	}
	// S3: unsettled binding, held-back records, an owner whose record just changed.
	in = base()
	in.BoundSince, in.HeldBack = now.Add(-time.Minute), 1
	if d := in.Deferred(); len(d) != 2 || !strings.HasPrefix(d[0], "unsettled") || !strings.HasPrefix(d[1], "held_back") {
		t.Fatalf("S3 deferred: %v", d)
	}
	in = base()
	in.MemberSince = func(id wire.PeerID) time.Time {
		if id == y {
			return now.Add(-time.Minute)
		}
		return now.Add(-time.Hour)
	}
	if conf, miss := in.Confirm([]wire.PeerID{x, y}, false); len(conf) != 1 || miss[0].Reason != "member_changed" {
		t.Fatalf("member changed: %v %v", conf, miss)
	}
	// S4: a displaced head within settle defers.
	in = base()
	in.Displaced = []string{"T2"}
	if d := in.Deferred(); len(d) != 1 || !strings.HasPrefix(d[0], "concurrent_heads") {
		t.Fatalf("S4: %v", d)
	}
	// Archive binding: a report bound to another live set is absent, an incomplete live
	// set is incomplete.
	in = base()
	in.Live = "L"
	in.Evidence[x] = Evidence{Report: Report{Target: "T", Live: "L", Policy: "P", Epoch: "E", Mode: ModeOwned, Complete: true}, Received: now}
	in.Evidence[y] = Evidence{Report: Report{Target: "T", Live: "L", Policy: "P", Epoch: "E", Mode: ModeOwned, Complete: true, Incomplete: []string{"v1"}}, Received: now}
	if conf, miss := in.Confirm([]wire.PeerID{x, y}, false); len(conf) != 1 || miss[0].Reason != ReasonIncomplete {
		t.Fatalf("live: %v %v", conf, miss)
	}
}
