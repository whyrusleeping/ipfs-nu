//go:build natlab

package netmon

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/natlab"
)

func TestNATLabReactionPipeline(t *testing.T) {
	if !natlab.Isolate(t) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m, err := Start(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	events, stop := m.Subscribe()
	defer stop()
	p, err := NewPipeline(100 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	for stage := Rebind; stage < Complete; stage++ {
		if _, err = p.Register(stage, func(context.Context, Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	results, unsub := p.Subscribe()
	defer unsub()
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	defer func() { cancel(); <-done }()
	// Drain the startup rescan, then make real netlink interface/address changes.
	for {
		select {
		case r := <-results:
			if r.Stage == Complete {
				goto ready
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
ready:
	start := time.Now()
	for _, args := range [][]string{{"link", "add", "test0", "type", "dummy"}, {"link", "set", "test0", "up"}, {"addr", "add", "10.88.0.1/24", "dev", "test0"}} {
		if out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip: %v: %s", err, out)
		}
	}
	deadline := time.NewTimer(time.Second - time.Since(start))
	defer deadline.Stop()
	var next Stage
	for {
		select {
		case r := <-results:
			if r.Event.Kind&Address == 0 {
				continue
			}
			if r.Stage != next || r.Err != nil {
				t.Fatalf("pipeline stage: %+v want %d", r, next)
			}
			next++
			if r.Stage == Complete {
				elapsed := time.Since(start)
				if elapsed >= time.Second {
					t.Fatal("reaction too slow", elapsed)
				}
				t.Logf("real interface/address change -> all six ordered hooks: %v", elapsed)
				return
			}
		case <-deadline.C:
			t.Fatal("pipeline did not finish within 1s")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
