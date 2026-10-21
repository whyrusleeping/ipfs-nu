package magicsock

import (
	"context"
	"net"
	"testing"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/qlog"
)

func TestCIDRetirement(t *testing.T) {
	c, err := New(Config{UDP: socket(t)})
	check(t, err)
	defer c.Close()
	id := peerID(t)
	addr, err := c.AddPeer(id)
	check(t, err)
	cfg := c.QUICConfigForPeer(net.UDPAddrFromAddrPort(addr), &quic.Config{})
	if cfg.InitialPacketSize != 1200 || !cfg.DisablePathMTUDiscovery {
		t.Fatal("unsafe inner budget")
	}
	tr := cfg.Tracer(context.Background(), true, quic.ConnectionID{})
	rec := tr.AddProducer()
	first := quic.ConnectionIDFromBytes([]byte("firstcid"))
	next := quic.ConnectionIDFromBytes([]byte("next-cid"))
	rec.RecordEvent(qlog.PacketSent{Header: qlog.PacketHeader{SrcConnectionID: first}, Frames: []qlog.Frame{{Frame: &qlog.NewConnectionIDFrame{SequenceNumber: 1, ConnectionID: next}}}})
	if c.Stats().CIDs != 2 {
		t.Fatal("CID issuance not routed", c.Stats())
	}
	rec.RecordEvent(qlog.PacketReceived{Frames: []qlog.Frame{{Frame: &qlog.RetireConnectionIDFrame{SequenceNumber: 1}}}})
	if c.Stats().CIDs != 1 {
		t.Fatal("retired CID retained")
	}
	check(t, rec.Close())
	if c.Stats().CIDs != 0 {
		t.Fatal("closed connection retained CIDs")
	}
}
