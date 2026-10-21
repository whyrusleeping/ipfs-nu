package circuit_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/relay"
)

func checked(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func newHost(t *testing.T) *host.Host {
	t.Helper()
	key, err := wire.GenerateNodeKey()
	checked(t, err)
	h, err := host.New(host.Config{Key: key})
	checked(t, err)
	t.Cleanup(func() { h.Close() })
	return h
}
func newClient(t *testing.T, h *host.Host) *circuit.Client {
	t.Helper()
	c, err := circuit.NewClient(h, circuit.ClientConfig{Timeout: time.Second})
	checked(t, err)
	t.Cleanup(func() { c.Close() })
	return c
}
func newRelay(t *testing.T, cfg circuit.ServerConfig) *relay.Server {
	t.Helper()
	key, err := wire.GenerateNodeKey()
	checked(t, err)
	r, err := relay.New(relay.Config{Host: host.Config{Key: key, Listen: []string{"127.0.0.1:0"}}, Server: cfg})
	checked(t, err)
	t.Cleanup(func() { r.Close() })
	return r
}
func connect(t *testing.T, ctx context.Context, h, to *host.Host) {
	t.Helper()
	a, err := wire.ParseMultiaddr(to.Addrs()[0].String()+"/p2p/"+to.ID().String(), 1024)
	checked(t, err)
	_, err = h.Dial(ctx, a)
	checked(t, err)
}
func TestCircuitRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := newRelay(t, circuit.ServerConfig{})
	a, b := newHost(t), newHost(t)
	source, target := newClient(t, a), newClient(t, b)
	connect(t, ctx, a, r.Host)
	connect(t, ctx, b, r.Host)
	if _, err := source.Connect(ctx, r.Host.ID(), b.ID()); !errors.Is(err, circuit.NoReservation) {
		t.Fatal("connected to unreserved target:", err)
	}
	reservation, err := target.Reserve(ctx, r.Host.ID())
	checked(t, err)
	checked(t, circuit.VerifyVoucher(reservation.Voucher, r.Host.ID(), b.ID(), reservation.Expires))
	if circuit.VerifyVoucher(reservation.Voucher, r.Host.ID(), a.ID(), reservation.Expires) == nil {
		t.Fatal("voucher does not bind reserving peer")
	}
	renewed, err := target.Reserve(ctx, r.Host.ID())
	checked(t, err)
	if renewed.Expires.Before(reservation.Expires) {
		t.Fatal("renewal moved expiry backwards")
	}
	done := make(chan error, 1)
	go func() {
		incoming, err := target.Accept(ctx)
		if err != nil {
			done <- err
			return
		}
		defer incoming.Close()
		if incoming.Peer != a.ID() || incoming.Relay != r.Host.ID() {
			done <- errors.New("wrong STOP metadata")
			return
		}
		_, err = io.Copy(incoming, incoming)
		done <- err
	}()
	stream, err := source.Connect(ctx, r.Host.ID(), b.ID())
	checked(t, err)
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(time.Second))
	payload := []byte("raw circuit stream, not a secure host connection")
	_, err = stream.Write(payload)
	checked(t, err)
	checked(t, stream.CloseWrite())
	got, err := io.ReadAll(stream)
	checked(t, err)
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo: %q", got)
	}
	select {
	case err := <-done:
		checked(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
func TestRelayLimitsAndAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a, b, other := newHost(t), newHost(t), newHost(t)
	source, target, outsider := newClient(t, a), newClient(t, b), newClient(t, other)
	r := newRelay(t, circuit.ServerConfig{MaxReservations: 2, ReservationsPerIP: 1, MaxCircuits: 1, Limited: circuit.Limit{Duration: 1, Data: 32}, Admitted: []wire.PeerID{a.ID(), b.ID()}})
	for _, h := range []*host.Host{a, b, other} {
		connect(t, ctx, h, r.Host)
	}
	reservation, err := target.Reserve(ctx, r.Host.ID())
	checked(t, err)
	if reservation.Limit != (circuit.Limit{}) {
		t.Fatal("admitted reservation was limited")
	}
	if _, err = outsider.Reserve(ctx, r.Host.ID()); !errors.Is(err, circuit.ReservationRefused) {
		t.Fatal("per-IP reservation cap:", err)
	}
	stream, err := source.Connect(ctx, r.Host.ID(), b.ID())
	checked(t, err)
	incoming, err := target.Accept(ctx)
	checked(t, err)
	if stream.Limit != (circuit.Limit{}) {
		t.Fatal("admitted pair limited")
	}
	if _, err = outsider.Connect(ctx, r.Host.ID(), b.ID()); !errors.Is(err, circuit.ResourceLimit) {
		t.Fatal("circuit slots:", err)
	}
	// Full-service peers can send beyond the limited service's byte cap.
	_, err = stream.Write(bytes.Repeat([]byte{'x'}, 64))
	checked(t, err)
	_ = incoming.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	_, err = io.ReadFull(incoming, buf)
	checked(t, err)
	checked(t, stream.Close())
	checked(t, incoming.Close())
	// A separate relay avoids timing-dependent slot-release polling. Admission
	// of the destination alone must not turn an outsider into a full-service user.
	limited := newRelay(t, circuit.ServerConfig{Limited: circuit.Limit{Duration: 1, Data: 32}, Admitted: []wire.PeerID{b.ID()}})
	connect(t, ctx, b, limited.Host)
	connect(t, ctx, other, limited.Host)
	_, err = target.Reserve(ctx, limited.Host.ID())
	checked(t, err)
	stream, err = outsider.Connect(ctx, limited.Host.ID(), b.ID())
	checked(t, err)
	defer stream.Close()
	incoming, err = target.Accept(ctx)
	checked(t, err)
	defer incoming.Close()
	if stream.Limit.Data != 32 || stream.Limit.Duration != 1 {
		t.Fatal("outsider received full service")
	}
	_, err = stream.Write(bytes.Repeat([]byte{'x'}, 33))
	checked(t, err)
	_ = incoming.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	got, err := io.ReadAll(incoming)
	var timeout net.Error
	if err == nil || len(got) > 32 || (errors.As(err, &timeout) && timeout.Timeout()) {
		t.Fatalf("directional cap not enforced: %d bytes, %v", len(got), err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err = stream.Read(buf[:1])
	if err == nil || (errors.As(err, &timeout) && timeout.Timeout()) {
		t.Fatalf("byte cap did not reset source leg: %v", err)
	}
}
