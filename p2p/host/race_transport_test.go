package host_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestSessionTransportColdRelayAndJoinedLoser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a, _ := sessionHost(t, nil, "")
	b, _ := sessionHost(t, nil, "")
	relay, _ := sessionHost(t, nil, "")
	direct, err := lookup.NormalizeAddress(b.ID(), b.Addrs()[0])
	if err != nil {
		t.Fatal(err)
	}
	circuit, err := wire.ParseMultiaddr(relay.Addrs()[0].String()+"/p2p/"+relay.ID().String()+"/p2p-circuit/p2p/"+b.ID().String(), 2048)
	if err != nil {
		t.Fatal(err)
	}
	directStarted := make(chan struct{})
	var joined atomic.Bool
	sessions, err := host.NewSessions(a, host.SessionConfig{
		Stagger: time.Second, // a cold relay must bypass this delay
		Peers:   map[wire.PeerID][]wire.Multiaddr{b.ID(): {direct, circuit}},
		Dial: func(ctx context.Context, peer wire.PeerID, addr wire.Multiaddr) (*host.Conn, error) {
			if peer != b.ID() {
				return nil, wire.ErrSignature
			}
			if host.IsCircuitAddress(addr) {
				select {
				case <-directStarted:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return a.Dial(ctx, direct) // transport translation, authenticated by Host
			}
			close(directStarted)
			<-ctx.Done()
			joined.Store(true)
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	sess, err := sessions.Connect(ctx, b.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !joined.Load() {
		t.Fatal("returned before losing transport dial joined")
	}
	if again, err := sessions.Get(b.ID()); err != nil || again != sess {
		t.Fatal("lost canonical session", err)
	}
}

func TestSessionTransportCannotBypassGatesOrIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	a, _ := sessionHost(t, nil, "")
	b, _ := sessionHost(t, nil, "")
	other, _ := sessionHost(t, nil, "")
	var calls atomic.Int32
	sessions, err := host.NewSessions(a, host.SessionConfig{
		Private: true, Peers: map[wire.PeerID][]wire.Multiaddr{b.ID(): b.Addrs()},
		Dial: func(ctx context.Context, _ wire.PeerID, _ wire.Multiaddr) (*host.Conn, error) {
			calls.Add(1)
			// Return a connection from a different host; even an authenticated connection
			// must belong to this manager and its expected destination.
			addr, err := lookup.NormalizeAddress(b.ID(), b.Addrs()[0])
			if err != nil {
				return nil, err
			}
			return other.Dial(ctx, addr)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	if _, err := sessions.Connect(ctx, other.ID()); !errors.Is(err, host.ErrPeerDenied) || calls.Load() != 0 {
		t.Fatal("private guard reached transport", err, calls.Load())
	}
	if _, err := sessions.Connect(ctx, b.ID()); !errors.Is(err, wire.ErrSignature) {
		t.Fatal("accepted foreign transport connection", err)
	}
	if calls.Load() != 1 {
		t.Fatal("transport not called")
	}
}
