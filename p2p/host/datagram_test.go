package host_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func datagramHost(t *testing.T, packet uint16) *host.Host {
	t.Helper()
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	h, err := host.New(host.Config{Key: key, Listen: []string{"127.0.0.1:0"}, InitialPacketSize: packet, DisablePathMTUDiscovery: packet != 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}
func dialDatagram(t *testing.T, ctx context.Context, from, to *host.Host) (*host.Conn, *host.Conn) {
	t.Helper()
	addr, err := wire.ParseMultiaddr(to.Addrs()[0].String()+"/p2p/"+to.ID().String(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	out, err := from.Dial(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	in, err := to.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return out, in
}

// The relay extension needs channel(2)+1200 = 1202 payload bytes. The default
// 1280-byte outer packet carries it; a 1200-byte outer budget (1163 bytes) must
// report ErrDatagramTooLarge so callers keep the reliable circuit stream.
func TestDatagramChannelsAndBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := datagramHost(t, 0), datagramHost(t, 0)
	out, in := dialDatagram(t, ctx, a, b)
	if !out.SupportsDatagrams() || !in.SupportsDatagrams() {
		t.Fatal("datagrams not negotiated")
	}
	rtt, err := out.ProbeDatagram(ctx, 1202)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.ProbeDatagram(ctx, 1300); !errors.Is(err, host.ErrDatagramTooLarge) {
		t.Fatal("1300-byte probe accepted on a 1280-byte outer budget:", err)
	}
	ch, err := in.OpenDatagramChannel(7, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	if _, err := in.OpenDatagramChannel(7, 8); !errors.Is(err, host.ErrChannelInUse) {
		t.Fatal("duplicate channel accepted")
	}
	body := make([]byte, 1200)
	body[0] = 0x40
	body[1199] = 9
	if err := out.SendDatagram(7, body); err != nil {
		t.Fatal(err)
	}
	if err := out.SendDatagram(8, body); err != nil { // unknown channel: dropped, counted
		t.Fatal(err)
	}
	got, err := ch.Receive(ctx)
	if err != nil || len(got) != 1200 || got[0] != 0x40 || got[1199] != 9 {
		t.Fatal("channel body", err, len(got))
	}
	deadline := time.Now().Add(2 * time.Second)
	for in.DatagramsDropped() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if in.DatagramsDropped() == 0 {
		t.Fatal("unknown channel not counted")
	}
	ch.Close()
	if _, err := ch.Receive(ctx); err == nil {
		t.Fatal("closed channel receive succeeded")
	}
	t.Logf("1202-byte probe echoed in %s on the default outer budget", rtt)

	small := datagramHost(t, 1200)
	sOut, _ := dialDatagram(t, ctx, small, b)
	if _, err := sOut.ProbeDatagram(ctx, 1202); !errors.Is(err, host.ErrDatagramTooLarge) {
		t.Fatal("1200-byte outer budget carried a 1202-byte probe:", err)
	}
	if _, err := sOut.ProbeDatagram(ctx, 1163); err != nil {
		t.Fatal("1163-byte probe on 1200-byte budget:", err)
	}
}
