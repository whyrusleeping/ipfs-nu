//go:build natlab

package netmon

import (
	"context"
	"os/exec"
	"reflect"
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
	type receipt struct {
		stages   []Stage
		finished time.Time
	}
	ready := make(chan struct{}, 1)
	completed := make(chan receipt, 1)
	// Observe actual hook executions, not the lossy diagnostics subscription.
	// Creating/raising a link also emits IPv6 and route notifications. Previously
	// those reactions filled the 32-entry diagnostics queue while this test was
	// busy running ip commands, dropping the beginning of an address reaction.
	var stages []Stage // only accessed by the serial pipeline goroutine
	for stage := Rebind; stage < Complete; stage++ {
		_, err = p.Register(stage, func(_ context.Context, e Event) error {
			if stage == Rebind {
				stages = stages[:0]
			}
			stages = append(stages, stage)
			if stage == RestartProbing {
				if e.Kind&Resync != 0 {
					select {
					case ready <- struct{}{}:
					default:
					}
				}
				if e.Kind&Address != 0 {
					result := receipt{append([]Stage(nil), stages...), time.Now()}
					select {
					case completed <- result:
					default:
					} // retain the first full address reaction
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, events) }()
	defer func() { cancel(); <-done }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	start := time.Now()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for _, args := range [][]string{{"link", "add", "test0", "type", "dummy"}, {"link", "set", "test0", "up"}, {"addr", "add", "10.88.0.1/24", "dev", "test0"}} {
		if out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip: %v: %s", err, out)
		}
	}
	select {
	case result := <-completed:
		want := []Stage{Rebind, ConfirmAddresses, RefreshMappings, RenewRelay, IdentifyPush, RestartProbing}
		if !reflect.DeepEqual(result.stages, want) {
			t.Fatal("hook execution order", result.stages)
		}
		elapsed := result.finished.Sub(start)
		if elapsed < 0 || elapsed >= time.Second {
			t.Fatal("reaction outside 1s window", elapsed)
		}
		t.Logf("real interface/address change -> all six ordered hooks: %v", elapsed)
	case <-deadline.C:
		t.Fatal("pipeline did not finish within 1s")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
