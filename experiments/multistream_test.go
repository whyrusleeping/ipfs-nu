package experiments_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	libhost "github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	libquic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func pair(t *testing.T) (*host.Host, libhost.Host) {
	t.Helper()
	upstream, err := libp2p.New(libp2p.NoTransports, libp2p.Transport(libquic.NewTransport), libp2p.ListenAddrStrings("/ip4/127.0.0.1/udp/0/quic-v1"), libp2p.DisableRelay())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	nu, err := host.New(host.Config{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nu.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addr, err := wire.ParseMultiaddr(upstream.Addrs()[0].String()+"/p2p/"+upstream.ID().String(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nu.Dial(ctx, addr); err != nil {
		t.Fatal(err)
	}
	return nu, upstream
}
func TestMultistreamSelect(t *testing.T) {
	nu, upstream := pair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	const p = protocol.ID("/nu/select-test/1")
	upstream.SetStreamHandler(p, func(s network.Stream) {
		defer s.Close()
		_ = s.SetDeadline(time.Now().Add(time.Second))
		b, err := wire.ReadMessage(s, 1024)
		if err == nil {
			_ = wire.WriteMessage(s, b, 1024)
		}
	})
	id, err := wire.ParsePeerID(upstream.ID().String())
	if err != nil {
		t.Fatal(err)
	}
	s, err := nu.OpenStreamLazy(ctx, id, string(p))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(time.Second))
	// Write before Confirm/Read: header, proposal and first framed request are
	// pipelined without waiting for a negotiation round trip.
	if err := wire.WriteMessage(s, []byte("optimistic"), 1024); err != nil {
		t.Fatal(err)
	}
	b, err := wire.ReadMessage(s, 1024)
	if err != nil || string(b) != "optimistic" {
		t.Fatal("lazy select", err)
	}
	rejected, err := nu.OpenStreamLazy(ctx, id, "/nu/unknown/1")
	if err != nil {
		t.Fatal(err)
	}
	defer rejected.Reset()
	_ = rejected.SetDeadline(time.Now().Add(time.Second))
	if err := rejected.Confirm(); !errors.Is(err, wire.ErrNotSupported) {
		t.Fatal("na response", err)
	}
	if err := nu.Handle(string(p), 2, func(s *host.Stream) {
		var b [4]byte
		if _, err := io.ReadFull(s, b[:]); err == nil {
			_, _ = s.Write(b[:])
		}
	}); err != nil {
		t.Fatal(err)
	}
	nuID, err := peer.Decode(nu.ID().String())
	if err != nil {
		t.Fatal(err)
	}
	remote, err := upstream.NewStream(ctx, nuID, p)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	_ = remote.SetDeadline(time.Now().Add(time.Second))
	if _, err := remote.Write([]byte("back")); err != nil {
		t.Fatal(err)
	}
	var response [4]byte
	if _, err := io.ReadFull(remote, response[:]); err != nil || string(response[:]) != "back" {
		t.Fatal("listener routing", err)
	}
}
