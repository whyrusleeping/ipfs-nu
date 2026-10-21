//go:build linux && natlab

package natlab

import (
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestCircuitToNATTarget(t *testing.T) {
	if !Isolate(t) {
		return
	}
	begin := time.Now()
	w := topology(t)
	_, err := w.router.NAT(NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr("198.18.0.20")})
	check(t, err)
	relay := start(t, w.public, HostConfig{Listen: "198.18.0.10:4000", CircuitServer: true, CircuitDuration: 1, CircuitTTL: time.Second})
	target := start(t, w.private, HostConfig{Listen: "10.0.0.2:4000", CircuitClient: true})
	source := start(t, w.public, HostConfig{Listen: "198.18.0.11:4000", CircuitClient: true})
	check(t, target.ReserveCircuit(relay.Address))
	observed, err := relay.Remote(target.ID)
	check(t, err)
	if endpoint(observed).Addr().String() != "198.18.0.20" {
		t.Fatal("reservation bypassed NAT:", observed)
	}
	got, err := source.EchoCircuit(relay.Address, target.ID, "NAT target via circuit v2")
	check(t, err)
	if got != "NAT target via circuit v2" {
		t.Fatal(got)
	}
	// Exercise actual renewal scheduling before TTL (not two immediate RESERVEs).
	check(t, target.MaintainCircuit(relay.Address))
	started := time.Now()
	err = source.IdleCircuit(relay.Address, target.ID)
	elapsed := time.Since(started)
	if err == nil || errors.Is(err, ErrTimeout) || elapsed < 900*time.Millisecond || elapsed > 1800*time.Millisecond {
		t.Fatalf("duration cap: elapsed %s, err %v", elapsed, err)
	}
	emit(t, Outcome{Scenario: "circuit-v2-nat-renewal-duration", From: "public-client", To: "nat-target", Outcome: "verified", Elapsed: time.Since(begin)})
}
