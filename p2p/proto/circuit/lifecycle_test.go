package circuit_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestMaintainNoticesLostReservationConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r := newRelay(t, circuit.ServerConfig{})
	h := newHost(t)
	c := newClient(t, h)
	connect(t, ctx, h, r.Host)
	reserved := make(chan circuit.Reservation, 1)
	done := make(chan error, 1)
	go func() { done <- c.Maintain(ctx, r.Host.ID(), func(r circuit.Reservation) { reserved <- r }) }()
	select {
	case <-reserved:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	r.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal("reservation loss was not detected", err)
		}
	case <-ctx.Done():
		t.Fatal("maintain waited for half-TTL after disconnect", ctx.Err())
	}
}

func TestCircuitRouteBinding(t *testing.T) {
	relay, peer := newHost(t), newHost(t)
	prefix, err := wire.ParseMultiaddr(relay.Addrs()[0].String()+"/p2p/"+relay.ID().String(), 1024)
	checked(t, err)
	addr, err := circuit.Address(prefix, peer.ID())
	checked(t, err)
	got, id, destination, err := circuit.ParseAddress(addr)
	checked(t, err)
	if got.String() != prefix.String() || id != relay.ID() || destination != peer.ID() {
		t.Fatal("route not bound", got, id, destination)
	}
	for _, text := range []string{
		prefix.String(),
		addr.String() + "/p2p-circuit/p2p/" + relay.ID().String(),
		relay.Addrs()[0].String() + "/p2p-circuit/p2p/" + peer.ID().String(),
	} {
		a, err := wire.ParseMultiaddr(text, 2048)
		checked(t, err)
		if _, _, _, err := circuit.ParseAddress(a); err == nil {
			t.Fatal("accepted unsupported relay route", a)
		}
	}
}
