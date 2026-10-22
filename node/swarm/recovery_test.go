package swarm

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/relay"
)

func TestCrossedCircuitAndActiveStreamFailover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ka, kb := swarmKey(t), swarmKey(t)
	r1, r2 := relayFor(t, ka.Public().PeerID(), kb.Public().PeerID()), relayFor(t, ka.Public().PeerID(), kb.Public().PeerID())
	addrs := []wire.Multiaddr{relayAddr(t, r1), relayAddr(t, r2)}
	a, b := testSwarm(t, ka, addrs...), testSwarm(t, kb, addrs...)
	ah, bh := circuitHints(t, ctx, a, 2), circuitHints(t, ctx, b, 2)
	done := make(chan error, 2)
	go func() { done <- a.Connect(ctx, b.ID(), bh) }()
	go func() { done <- b.Connect(ctx, a.ID(), ah) }()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal("crossed connection", err)
		}
	}
	before, ok := a.host.Connection(b.ID())
	if !ok {
		t.Fatal("missing inner")
	}
	if err := b.host.Handle("/test/recover/1", 1, func(s *host.Stream) { io.Copy(s, s) }); err != nil {
		t.Fatal(err)
	}
	stream, err := a.sessions.OpenStream(ctx, b.ID(), "/test/recover/1")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	stream.SetDeadline(time.Now().Add(6 * time.Second))
	chunk := make([]byte, 32<<10)
	received := make([]byte, len(chunk))
	round := func() {
		t.Helper()
		if _, err := stream.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(stream, received); err != nil {
			t.Fatal("active stream failed", err)
		}
	}
	round()
	p, err := a.connectivity.peer(b.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.lock(ctx); err != nil {
		t.Fatal(err)
	}
	home := p.relay
	p.unlock()
	for _, r := range []*relay.Server{r1, r2} {
		if r.Host.ID() == home {
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for range 8 {
		round()
	}
	after, ok := a.host.Connection(b.ID())
	if !ok || after != before {
		t.Fatal("replaced inner connection instead of repairing packet path")
	}
	t.Log("crossed initiation converged; active stream survived home relay loss on the same authenticated inner QUIC connection")
}
