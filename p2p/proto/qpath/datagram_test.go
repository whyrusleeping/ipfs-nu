package qpath_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/magicsock"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/proto/qpath"
)

// acceptDatagram attaches the relay's offer to B's live stream leg, as the
// connectivity owner does.
func (x *setup) acceptDatagram(t *testing.T, ctx context.Context) {
	t.Helper()
	go func() {
		for {
			dl, err := x.bc.AcceptDatagram(ctx)
			if err != nil {
				return
			}
			_ = qpath.AttachDatagram(x.relayB, dl, x.r.ID(), x.a.ID())
		}
	}()
}
func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDatagramRelayUpgradeAndFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	x := makeSetup(t, ctx, udp(t, "127.0.0.1:0"), udp(t, "127.0.0.1:0"), udp(t, "127.0.0.1:0"))
	x.acceptDatagram(t, ctx)
	s := x.stream(t, ctx)
	roundtrip(t, s, 2)
	before, _ := x.a.Connection(x.b.ID())
	_, err := qpath.UpgradeDatagram(ctx, x.ac, x.relayA, x.r.ID(), x.b.ID())
	check(t, err)
	if _, err := qpath.UpgradeDatagram(ctx, x.ac, x.relayA, x.r.ID(), x.b.ID()); !errors.Is(err, magicsock.ErrLegExists) {
		t.Fatal("second upgrade accepted:", err)
	}
	waitFor(t, "B did not attach the offered leg", func() bool { set, _ := x.mb.Paths(x.a.ID()); return set.Datagram })
	roundtrip(t, s, 8)
	sa, sb := x.ma.Stats(), x.mb.Stats()
	if sa.DatagramSent == 0 || sb.DatagramSent == 0 || sa.DatagramReceived == 0 || sb.DatagramReceived == 0 {
		t.Fatal("datagram leg unused", sa, sb)
	}
	// Relay byte counters (0024 metrics) must include datagram-leg traffic in
	// both directions: a 96 KiB echo moved far more than the handshake bytes.
	if sa.Sent.Relay < 90<<10 || sa.Recv.Relay < 90<<10 || sb.Recv.Relay < 90<<10 || sa.Sent.Direct != 0 {
		t.Fatal("relay byte counters miss datagram-leg bytes", sa.Sent, sa.Recv, sb.Recv)
	}
	after, ok := x.a.Connection(x.b.ID())
	if !ok || after != before {
		t.Fatal("inner connection replaced by the upgrade")
	}
	t.Logf("datagram leg carried A=%d/%d B=%d/%d packets (sent/received); relay legs=%d", sa.DatagramSent, sa.DatagramReceived, sb.DatagramSent, sb.DatagramReceived, x.server.DatagramLegs())
	// Dropping the leg on one side tears the pair down at the relay; both ends
	// fall back to the stream and the inner connection survives.
	x.relayA.DetachPacketLeg()
	waitFor(t, "B kept a dead datagram leg", func() bool { set, _ := x.mb.Paths(x.a.ID()); return !set.Datagram })
	waitFor(t, "relay kept the pair", func() bool { return x.server.DatagramLegs() == 0 })
	relayedBefore := x.ma.Stats().Relayed - x.ma.Stats().DatagramSent
	roundtrip(t, s, 4)
	if x.ma.Stats().Relayed-x.ma.Stats().DatagramSent <= relayedBefore {
		t.Fatal("stream fallback unused after datagram teardown")
	}
	// Re-upgrade works after fallback (owner retry).
	_, err = qpath.UpgradeDatagram(ctx, x.ac, x.relayA, x.r.ID(), x.b.ID())
	check(t, err)
	roundtrip(t, s, 2)
	if again, _ := x.a.Connection(x.b.ID()); again != before {
		t.Fatal("inner connection replaced")
	}
}

// A 1200-byte outer packet budget yields 1163 payload bytes: the relay's probe
// must fail and the pair stays on the stream, with no truncation attempt.
func TestDatagramRelayBudgetRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	relayPacketSize = 1200
	t.Cleanup(func() { relayPacketSize = 0 })
	x := makeSetup(t, ctx, udp(t, "127.0.0.1:0"), udp(t, "127.0.0.1:0"), udp(t, "127.0.0.1:0"))
	x.acceptDatagram(t, ctx)
	s := x.stream(t, ctx)
	roundtrip(t, s, 2)
	_, err := qpath.UpgradeDatagram(ctx, x.ac, x.relayA, x.r.ID(), x.b.ID())
	if !errors.Is(err, circuit.ErrDatagramUnsupported) || !errors.Is(err, circuit.ConnectionFailed) {
		t.Fatal("small-budget relay did not refuse:", err)
	}
	if set, _ := x.ma.Paths(x.b.ID()); set.Datagram || !set.Relay {
		t.Fatal("unexpected path state", set)
	}
	roundtrip(t, s, 4)
	if x.ma.Stats().DatagramSent != 0 || x.server.DatagramLegs() != 0 {
		t.Fatal("datagrams used despite refusal")
	}
	t.Logf("1200-byte outer budget refused by probe: %v; stream relay continued", err)
}

// Non-admitted pairs and missing circuits are refused before any probing.
func TestDatagramRelayAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	x := makeSetup(t, ctx, udp(t, "127.0.0.1:0"), udp(t, "127.0.0.1:0"), udp(t, "127.0.0.1:0"))
	if _, err := x.ac.UpgradeDatagram(ctx, x.r.ID(), x.a.ID()); !errors.Is(err, circuit.PermissionDenied) {
		t.Fatal("self pair accepted:", err)
	}
	if _, err := x.ac.UpgradeDatagram(ctx, x.r.ID(), x.r.ID()); !errors.Is(err, circuit.PermissionDenied) {
		t.Fatal("relay as peer accepted:", err)
	}
	// B has no policy approval for A → relay reports the target's refusal.
	bcRefuse, err := circuit.NewClient(x.b, circuit.ClientConfig{})
	if err == nil {
		bcRefuse.Close()
		t.Fatal("second STOP handler registered")
	}
	check(t, x.relayA.Close())
	check(t, x.relayB.Close())
	waitFor(t, "pair not released", func() bool {
		_, err := x.ac.UpgradeDatagram(ctx, x.r.ID(), x.b.ID())
		return errors.Is(err, circuit.NoReservation)
	})
}
