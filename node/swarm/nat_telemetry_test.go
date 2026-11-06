package swarm

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/disco"
	"github.com/whyrusleeping/nu/p2p/netcheck"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Off by default; on, each probe session yields exactly the documented fields
// (no PeerID, address or time), and early-ended sessions are not recorded.
func TestNATTelemetry(t *testing.T) {
	r, _, err := repo.Open(filepath.Join(t.TempDir(), "nu"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err := RepoConfig(r, nil, nil); err != nil || cfg.NATTelemetry != "" {
		t.Fatal("telemetry not off by default", cfg.NATTelemetry, err)
	}
	if err := r.ConfigSet("swarm.nat_telemetry", "on"); err != nil {
		t.Fatal(err)
	}
	cfg, err := RepoConfig(r, nil, nil)
	if err != nil || cfg.NATTelemetry != r.Path(repo.NATTelemetryFile) {
		t.Fatal(cfg.NATTelemetry, err)
	}
	if err := applyConnectivityConfig(&Config{}, repo.SwarmConfig{NATTelemetry: "yes"}); err == nil {
		t.Fatal("accepted a malformed swarm.nat_telemetry")
	}

	tel := newNATTelemetry(cfg.NATTelemetry)
	tel.status = func(wire.PeerID) (disco.Status, bool) { return disco.Status{HardAttempts: 1}, true }
	tel.local = func() (netcheck.Profile, bool) {
		return netcheck.Profile{UDP: netcheck.UDPOK, Mapping: netcheck.MappingEndpointIndependent, Filtering: netcheck.FilterAddressPortDependent}, true
	}
	tel.remote = func(context.Context, wire.PeerID) (netcheck.Profile, bool) {
		return netcheck.Profile{UDP: netcheck.UDPOK, Mapping: netcheck.MappingAddressPortDependent, Filtering: netcheck.FilterAddressPortDependent, Allocation: netcheck.AllocRandom}, true
	}
	a, b, c := swarmKey(t).Public().PeerID(), swarmKey(t).Public().PeerID(), swarmKey(t).Public().PeerID()
	tel.observe(disco.Event{Peer: a, Kind: disco.SessionStarted})
	tel.observe(disco.Event{Peer: a, Kind: disco.PathDirect, Elapsed: 5 * time.Second, Hard: true})
	tel.observe(disco.Event{Peer: a, Kind: disco.PathDirect, Elapsed: 9 * time.Second}) // only the first counts
	tel.observe(disco.Event{Peer: b, Kind: disco.SessionStarted})
	tel.expire(b, tel.open[b])
	tel.observe(disco.Event{Peer: c, Kind: disco.SessionStarted})
	tel.observe(disco.Event{Peer: c, Kind: disco.SessionEnded})
	if len(tel.queue) != 2 {
		t.Fatal("want 2 queued records, got", len(tel.queue))
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for tel.written.Load() < 2 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	tel.run(ctx)
	data, err := os.ReadFile(cfg.NATTelemetry)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatal(lines)
	}
	want := []map[string]any{
		{"scenario": "field", "from": "EIM/APDF", "to": "APDM-random/APDF", "strategy": "hard-nat", "outcome": "direct", "time_to_direct_ns": 5e9, "nu": nu.Version},
		{"scenario": "field", "from": "EIM/APDF", "to": "APDM-random/APDF", "strategy": "hard-nat", "outcome": "relayed", "nu": nu.Version},
	}
	for i, line := range lines {
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want[i]) {
			t.Fatalf("record %d has fields %v, want exactly %v", i, slices.Sorted(maps.Keys(got)), want[i])
		}
		for k, v := range want[i] {
			if got[k] != v {
				t.Fatalf("record %d: %s = %v, want %v", i, k, got[k], v)
			}
		}
	}
}
