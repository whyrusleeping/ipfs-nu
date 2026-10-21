package nutest_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/fetch"
	"github.com/whyrusleeping/nu/nutest"
)

// small requests so a 1 MiB transfer has many of them.
var smallRequests = fetch.Config{ChunkGroups: 4, PeerRequests: 1}

// Three nodes start fast; n2 fetches from n0 and n1 while n0 restarts mid-transfer, and
// the content survives on every node, including n0's store across the restart.
func TestRestartMidTransfer(t *testing.T) {
	started := time.Now()
	n := nutest.New(t, 3, nutest.Options{Fetch: smallRequests})
	// Startup is normally tens of milliseconds; the bound only catches gross regressions
	// and stays loose because the full suite runs packages in parallel on shared CPUs.
	if d := time.Since(started); d > 10*time.Second {
		t.Fatalf("starting 3 nodes took %s", d)
	} else {
		t.Logf("started 3 nodes in %s", d)
	}
	n0, n1, n2 := n.Nodes[0], n.Nodes[1], n.Nodes[2]
	data := n.RandBytes(1 << 20)
	c := n0.AddBytes(data)
	n1.Get(n.Link(c, n0), filepath.Join(t.TempDir(), "copy"))
	if !n1.Has(c) {
		t.Fatal("n1 does not hold the content after get")
	}

	var once sync.Once
	restarted := make(chan struct{})
	err := n2.Fetch(c, func(done, total int64) {
		if done >= 128<<10 && done < total {
			once.Do(func() {
				go func() { defer close(restarted); n0.Restart() }()
			})
		}
	}, n0, n1)
	if err != nil {
		t.Fatalf("fetch across restart: %v", err)
	}
	select {
	case <-restarted:
	case <-time.After(nutest.Timeout):
		t.Fatal("n0 did not restart")
	}
	for _, nd := range n.Nodes {
		if !nd.Has(c) {
			t.Fatalf("%s lost the content", nd.Name)
		}
	}
	if got, err := n2.Cat(c.String()); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("fetched bytes differ (err %v)", err)
	}
	// Killed (not stopped) nodes come back with their store too.
	n1.Kill()
	n1.Start()
	if !n1.Has(c) {
		t.Fatal("n1 lost the content across a kill")
	}
}

// A partition closes the connection and makes fetches fail with unavailable; healing
// lets the same fetch succeed.
func TestPartitionHeal(t *testing.T) {
	n := nutest.New(t, 2)
	n0, n1 := n.Nodes[0], n.Nodes[1]
	c := n0.AddBytes(n.RandBytes(64 << 10))
	n.Peer(n1, n0)
	n.Partition(n0, n1)
	nutest.Eventually(t, "partition to close the connection", func() bool { return !n1.ConnectedTo(n0.ID) && !n0.ConnectedTo(n1.ID) })
	err := n1.Fetch(c, nil, n0)
	var ae *apiv0.Error
	if !errors.As(err, &ae) || ae.Code != apiv0.CodeUnavailable {
		t.Fatalf("fetch across partition: %v", err)
	}
	n.Heal(n0, n1)
	if err := n1.Fetch(c, nil, n0); err != nil || !n1.Has(c) {
		t.Fatalf("fetch after heal: %v", err)
	}
}

// A provider that serves corrupt bytes is detected and dropped: with another provider
// the fetch completes with correct bytes; alone, it fails.
func TestCorruptProvider(t *testing.T) {
	n := nutest.New(t, 3, nutest.Options{Fetch: smallRequests})
	bad, good, client := n.Nodes[0], n.Nodes[1], n.Nodes[2]
	data := n.RandBytes(256 << 10)
	c := bad.AddBytes(data)
	only := bad.AddBytes(n.RandBytes(64 << 10))
	good.Get(n.Link(c, bad), filepath.Join(t.TempDir(), "copy"))
	bad.Corrupt(true)

	if err := client.Fetch(only, nil, bad); err == nil || client.Has(only) {
		t.Fatalf("fetch from a corrupt-only provider: %v", err)
	}
	if bad.CorruptServed.Load() == 0 {
		t.Fatal("the corrupt provider served nothing")
	}
	if err := client.Fetch(c, nil, bad, good); err != nil {
		t.Fatalf("fetch with a corrupt and a good provider: %v", err)
	}
	if got, err := client.Cat(c.String()); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("fetched bytes differ (err %v)", err)
	}
}
