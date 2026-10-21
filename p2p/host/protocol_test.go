package host_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestProtocolConcurrency(t *testing.T) {
	makeHost := func() *host.Host {
		k, err := wire.GenerateNodeKey()
		if err != nil {
			t.Fatal(err)
		}
		h, err := host.New(host.Config{Key: k})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Close() })
		return h
	}
	a, b := makeHost(), makeHost()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	if err := b.Handle("/nu/bounded/1", 1, func(s *host.Stream) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-s.Conn().Context().Done():
		}
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := a.Dial(ctx, address(t, b, b.ID())); err != nil {
		t.Fatal(err)
	}
	first, err := a.OpenStream(ctx, b.ID(), "/nu/bounded/1")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if second, err := a.OpenStream(ctx, b.ID(), "/nu/bounded/1"); !errors.Is(err, wire.ErrNotSupported) {
		if second != nil {
			second.Reset()
		}
		t.Fatal("per-protocol limit", err)
	}
	close(release)
}
