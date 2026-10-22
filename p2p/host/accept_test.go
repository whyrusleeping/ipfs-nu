package host_test

import (
	"context"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/nutest/timeouts"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// A dial that the responder's connection policy refuses reports an error, never a
// transient success (0076): the dialer waits for the responder to act on its
// certificate before returning. Repeated to catch the race the old code lost.
func TestRefusedDialNeverLooksConnected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.Ceiling())
	defer cancel()
	k, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	member, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	private, err := host.New(host.Config{Key: k, ManualStreams: true, AllowPeer: func(id wire.PeerID) bool { return id == member.Public().PeerID() }})
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	for i := range 10 {
		outsider := newHost(t)
		started := time.Now()
		conn, derr := outsider.Dial(ctx, address(t, private, private.ID()))
		if derr == nil {
			_ = conn.Close()
			t.Fatalf("attempt %d: a refused dial returned a connection", i)
		}
		if _, ok := private.Connection(outsider.ID()); ok {
			t.Fatalf("attempt %d: the private host registered the outsider", i)
		}
		if _, ok := outsider.Connection(private.ID()); ok {
			t.Fatalf("attempt %d: the outsider kept a connection", i)
		}
		if d := time.Since(started); d > time.Second {
			t.Fatalf("attempt %d: refusal took %s", i, d)
		}
	}
	// An allowed dial still succeeds promptly.
	m, err := host.New(host.Config{Key: member, ManualStreams: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	started := time.Now()
	if _, err := m.Dial(ctx, address(t, private, private.ID())); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(started); d > time.Second {
		t.Fatalf("accepted dial took %s", d)
	}
}
