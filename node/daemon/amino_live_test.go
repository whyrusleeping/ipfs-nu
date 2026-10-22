package daemon_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/daemon"
	"github.com/whyrusleeping/nu/node/repo"
)

// TestAminoLiveProvideAndGet is the 0072 opt-in live check (NU_AMINO=1): a fresh daemon
// adds a file and provides it on the public Amino DHT; a second fresh daemon, given only
// the CID, finds the provider record through Amino and fetches the file. Both daemons
// run on this machine, so the provider address must be one this machine can dial
// itself; the check records whether that held (see docs/validation.md).
func TestAminoLiveProvideAndGet(t *testing.T) {
	if os.Getenv("NU_AMINO") == "" {
		t.Skip("set NU_AMINO=1 to run the live Amino check (needs internet access)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	start := func(name string) *daemon.Daemon {
		r, _, err := repo.Open(filepath.Join(t.TempDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		d, err := daemon.Start(ctx, daemon.Options{Repo: r, APIListen: "127.0.0.1:0", GatewayListen: "127.0.0.1:0", NoMDNS: true,
			SwarmListen: []string{"/ip4/0.0.0.0/udp/0/quic-v1"}, Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		return d
	}
	a := start("a")
	data := make([]byte, 300_000)
	for i := range data {
		data[i] = byte(i*7 + int(time.Now().UnixNano()))
	}
	file := filepath.Join(t.TempDir(), "live.bin")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	op, err := a.Node.Add(ctx, apiv0.AddRequest{Path: file, Pin: true})
	if err != nil {
		t.Fatal(err)
	}
	final, err := a.Node.WaitOperation(ctx, op.ID, time.Minute)
	if err != nil || final.State != apiv0.StateSucceeded {
		t.Fatalf("add: %v %+v", err, final)
	}
	var added apiv0.AddResult
	if err := json.Unmarshal(final.Result, &added); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Minute)
	for {
		st := a.Swarm.DHTStatus()
		if st.Provided >= 2 {
			t.Logf("A provided %d keys at %v (routing peers %d)", st.Provided, st.Announcing, st.RoutingPeers)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("A did not provide: %+v", st)
		}
		time.Sleep(time.Second)
	}

	b := start("b")
	out := filepath.Join(t.TempDir(), "out")
	started := time.Now()
	op, err = b.Node.Get(ctx, apiv0.GetRequest{Target: added.CID, Output: out})
	if err != nil {
		t.Fatal(err)
	}
	final, err = b.Node.WaitOperation(ctx, op.ID, 3*time.Minute)
	if err != nil || final.State != apiv0.StateSucceeded {
		t.Fatalf("get via Amino: %v %+v (B dht %+v)", err, final, b.Swarm.DHTStatus())
	}
	got, err := os.ReadFile(out)
	if err != nil || string(got) != string(data) {
		t.Fatalf("fetched bytes differ (%v)", err)
	}
	t.Logf("B fetched %s through Amino in %s (routing peers %d)", added.CID, time.Since(started).Round(time.Millisecond), b.Swarm.DHTStatus().RoutingPeers)
}
