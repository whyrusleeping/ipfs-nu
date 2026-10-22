package magicsock

import (
	"context"
	"net"
	"testing"

	quic "github.com/quic-go/quic-go"
)

func TestCIDRetirement(t *testing.T) {
	c, err := New(Config{UDP: socket(t)})
	check(t, err)
	defer c.Close()
	id := peerID(t)
	addr, err := c.AddPeer(id)
	check(t, err)
	var downstream []quic.ConnectionID
	base := &quic.Config{ConnectionIDTracer: func(context.Context, bool, quic.ConnectionID) *quic.ConnectionIDTracer {
		return &quic.ConnectionIDTracer{AddedConnectionID: func(id quic.ConnectionID) { downstream = append(downstream, id) }}
	}}
	cfg := c.QUICConfigForPeer(net.UDPAddrFromAddrPort(addr), base)
	if cfg.InitialPacketSize != 1200 || !cfg.DisablePathMTUDiscovery || cfg.Tracer != nil {
		t.Fatal("unsafe inner budget or qlog observer installed")
	}
	tr := cfg.ConnectionIDTracer(context.Background(), true, quic.ConnectionID{})
	other := cfg.ConnectionIDTracer(context.Background(), true, quic.ConnectionID{})
	first := quic.ConnectionIDFromBytes([]byte("firstcid"))
	next := quic.ConnectionIDFromBytes([]byte("next-cid"))
	tr.AddedConnectionID(first)
	tr.AddedConnectionID(next)
	if c.Stats().CIDs != 2 || len(downstream) != 2 {
		t.Fatal("CID issuance not routed", c.Stats(), downstream)
	}
	other.AddedConnectionID(next) // collision: must not steal or later remove the route
	other.RemovedConnectionID(next)
	if c.Stats().CIDs != 2 {
		t.Fatal("other connection removed a route it does not own")
	}
	tr.RemovedConnectionID(next)
	if c.Stats().CIDs != 1 {
		t.Fatal("retired CID retained")
	}
	tr.RemovedConnectionID(first)
	if c.Stats().CIDs != 0 {
		t.Fatal("closed connection retained CIDs")
	}
}
