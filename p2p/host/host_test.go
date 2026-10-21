package host_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func newHost(t *testing.T) *host.Host {
	t.Helper()
	k, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	h, err := host.New(host.Config{Key: k, ManualStreams: true, Limits: host.Limits{MaxConnections: 4, MaxStreamsPerConnection: 1}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}
func address(t *testing.T, h *host.Host, id wire.PeerID) wire.Multiaddr {
	t.Helper()
	a, err := wire.ParseMultiaddr(h.Addrs()[0].String()+"/p2p/"+id.String(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func TestDirectAuthentication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a, b := newHost(t), newHost(t)
	wrong, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	if c, err := a.Dial(ctx, address(t, b, wrong.Public().PeerID())); err == nil {
		_ = c.Close()
		t.Fatal("wrong PeerId accepted")
	}
	outgoing, err := a.Dial(ctx, address(t, b, b.ID()))
	if err != nil {
		t.Fatal(err)
	}
	incoming, err := b.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if outgoing.PeerID() != b.ID() || incoming.PeerID() != a.ID() {
		t.Fatal("peer authentication")
	}
	again, err := a.Dial(ctx, address(t, b, b.ID()))
	if err != nil || again != outgoing {
		t.Fatal("connection not reused", err)
	}
	stream, err := outgoing.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := outgoing.OpenStream(ctx); !errors.Is(err, host.ErrLimit) {
		t.Fatal("stream limit", err)
	}
	done := make(chan error, 1)
	go func() {
		stream, err := incoming.AcceptStream(ctx)
		if err != nil {
			done <- err
			return
		}
		defer stream.Close()
		_ = stream.SetDeadline(time.Now().Add(time.Second))
		b := make([]byte, 4)
		if _, err = io.ReadFull(stream, b); err == nil {
			_, err = stream.Write(b)
		}
		done <- err
	}()
	_ = stream.SetDeadline(time.Now().Add(time.Second))
	if _, err := stream.Write([]byte("test")); err != nil {
		t.Fatal(err)
	}
	var got [4]byte
	if _, err := io.ReadFull(stream, got[:]); err != nil || string(got[:]) != "test" {
		t.Fatal("bidirectional stream", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	next, err := outgoing.OpenStream(ctx)
	if err != nil {
		t.Fatal("stream slot not released", err)
	}
	next.Reset()
}
