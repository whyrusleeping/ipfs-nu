package experiments_test

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	libclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	librelay "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/relay"
)

func TestCircuitV2(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	nu, upstream := endpoints(t)
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	r, err := relay.New(relay.Config{Host: host.Config{Key: key, Listen: []string{"127.0.0.1:0"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	id, err := peer.Decode(r.Host.ID().String())
	if err != nil {
		t.Fatal(err)
	}
	// Actual upstream reservation client verifies our signed envelope and fields.
	reservation, err := libclient.Reserve(ctx, upstream, peer.AddrInfo{ID: id, Addrs: []ma.Multiaddr{ma.StringCast(r.Host.Addrs()[0].String())}})
	if err != nil || reservation.Voucher == nil || reservation.Voucher.Peer != upstream.ID() || reservation.Voucher.Relay != id || reservation.LimitData != 128<<10 {
		t.Fatal("go client -> Nu relay", reservation, err)
	}
	// Actual upstream relay signs a voucher which Nu must independently verify.
	remote, err := librelay.New(upstream, librelay.WithReservationAddressFilter(func(ma.Multiaddr) bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	target, err := circuit.NewClient(nu, circuit.ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	connectPair(t, nu, upstream)
	relayID, err := wire.ParsePeerID(upstream.ID().String())
	if err != nil {
		t.Fatal(err)
	}
	result, err := target.Reserve(ctx, relayID)
	if err != nil || len(result.Voucher) == 0 {
		t.Fatal("Nu client -> go relay", result, err)
	}
	if err = circuit.VerifyVoucher(result.Voucher, relayID, nu.ID(), result.Expires); err != nil {
		t.Fatal(err)
	}
	// Raw CONNECT/STOP byte streams through upstream: intentionally no claim
	// that stock libp2p's Noise/yamux secure-connection upgrade is implemented.
	sourceKey, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	sourceHost, err := host.New(host.Config{Key: sourceKey})
	if err != nil {
		t.Fatal(err)
	}
	defer sourceHost.Close()
	addr, err := wire.ParseMultiaddr(upstream.Addrs()[0].String()+"/p2p/"+upstream.ID().String(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sourceHost.Dial(ctx, addr); err != nil {
		t.Fatal(err)
	}
	source, err := circuit.NewClient(sourceHost, circuit.ClientConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	done := make(chan error, 1)
	go func() {
		s, err := target.Accept(ctx)
		if err != nil {
			done <- err
			return
		}
		defer s.Close()
		_, err = io.Copy(s, s)
		done <- err
	}()
	stream, err := source.Connect(ctx, relayID, nu.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(time.Second))
	payload := []byte("circuit-v2 byte stream through go-libp2p")
	if _, err = stream.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err = stream.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(stream)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal(string(got), err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
