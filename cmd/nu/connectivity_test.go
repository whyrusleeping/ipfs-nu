package main

import (
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
)

// Doctor explains a UDP-blocked node and a relayed session in plain language.
func TestDoctorConnectivityExplanations(t *testing.T) {
	blocked := &apiv0.ConnectivityStatus{Reachability: "unknown",
		UDP:    &apiv0.UDPStatus{Outbound: "blocked", RelaysConfigured: 2, Detail: "none of the 2 configured relays answered over UDP"},
		Relays: []apiv0.RelayStatus{{PeerID: "k51a", LastError: "dial timeout"}, {PeerID: "k51b", LastError: "dial timeout"}},
		NAT:    &apiv0.NATProfile{Mapping: "unknown", Filtering: "unknown", Source: "observations"}}
	var udp, relays check
	for _, c := range connectivityChecks(blocked) {
		switch {
		case c.Name == "udp":
			udp = c
		case c.Name == "home relays":
			relays = c
		}
	}
	if udp.Status != "fail" || !strings.Contains(udp.Detail, "UDP appears blocked") || !strings.Contains(udp.Action, "allow outbound UDP") {
		t.Fatalf("udp check: %+v", udp)
	}
	if relays.Status != "warn" {
		t.Fatalf("home relays check: %+v", relays)
	}
	relayed := &apiv0.ConnectivityStatus{Reachability: "private", Home: "k51a", Backup: "k51b",
		UDP: &apiv0.UDPStatus{Outbound: "ok", RelaysConfigured: 2, RelaysConnected: 2},
		NAT: &apiv0.NATProfile{Mapping: "endpoint-dependent", Filtering: "unknown", Source: "observations", Observers: 2},
		Paths: []apiv0.PeerPathStatus{{PeerID: "k51c", Path: "relay", Relay: "k51a", Candidates: 2, ProbesSent: 7,
			Why: "2 candidate endpoints, 7 probes sent, none answered", History: []apiv0.PathEvent{{Event: "relay circuit via k51a ready in 12ms"}}}}}
	var nat, session check
	for _, c := range connectivityChecks(relayed) {
		switch c.Name {
		case "nat":
			nat = c
		case "session k51c":
			session = c
		}
	}
	if nat.Status != "warn" || !strings.Contains(nat.Action, "router-mapped") {
		t.Fatalf("nat check: %+v", nat)
	}
	if session.Status != "warn" || !strings.Contains(session.Detail, "none answered") || !strings.Contains(session.Detail, "relay circuit via k51a") {
		t.Fatalf("session check: %+v", session)
	}
}
