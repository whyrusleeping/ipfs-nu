package fetch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/whyrusleeping/nu/p2p/host"
)

func TestConcurrentConnectionFailureBudget(t *testing.T) {
	s := &swarm{}
	p := &peer{}
	ctx := context.Background()
	for generation := 1; generation <= maxFails; generation++ {
		conn := new(host.Conn)
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.report(ctx, p, fmt.Errorf("range: %w", &connectionError{error: errors.New("closed"), conn: conn}))
			}()
		}
		wg.Wait()
		if p.fails != generation || p.bad != (generation == maxFails) || len(p.connectionFailures) != generation {
			t.Fatalf("generation %d: %+v", generation, p)
		}
	}
	ordinary := &peer{}
	for range maxFails {
		s.report(ctx, ordinary, errors.New("request failure"))
	}
	if !ordinary.bad {
		t.Fatal("ordinary failures lost their bound")
	}
}
