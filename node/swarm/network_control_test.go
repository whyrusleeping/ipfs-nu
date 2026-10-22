package swarm

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/disco"
	"github.com/whyrusleeping/nu/p2p/netmon"
)

func TestOwnerPolicyDirectUpgradeAndNetworkHooks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	ka, kb := swarmKey(t), swarmKey(t)
	r := relayFor(t, ka.Public().PeerID(), kb.Public().PeerID())
	a, b := testSwarm(t, ka, relayAddr(t, r)), testSwarm(t, kb, relayAddr(t, r))
	na, nb := a.connectivity.network, b.connectivity.network
	ea, eb := a.connectivity.socket.LocalAddr().(*net.UDPAddr).AddrPort(), b.connectivity.socket.LocalAddr().(*net.UDPAddr).AddrPort()
	// Explicit administrative exact-endpoint grants, independent of peer records.
	na.allow(b.ID(), eb)
	nb.allow(a.ID(), ea)
	for _, addr := range []netip.AddrPort{netip.MustParseAddrPort("10.0.0.1:22"), netip.AddrPortFrom(eb.Addr(), eb.Port()+1)} {
		if na.authorize(b.ID(), disco.Candidate{Addr: addr}) {
			t.Fatal("policy allowed an ungranted target", addr)
		}
	}
	if len(na.candidates(swarmKey(t).Public().PeerID())) != 0 {
		t.Fatal("LAN/loopback candidates disclosed to unknown relayed peer")
	}
	if err := a.Connect(ctx, b.ID(), circuitHints(t, ctx, b, 1)); err != nil {
		t.Fatal(err)
	}
	before, _ := a.host.Connection(b.ID())
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		pa, _ := a.connectivity.socket.Paths(b.ID())
		pb, _ := b.connectivity.socket.Paths(a.ID())
		if pa.Selected.IsValid() && pb.Selected.IsValid() {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("owner disco did not upgrade", ctx.Err())
		case <-ticker.C:
		}
	}
	if _, err := a.Ping(ctx, b.ID()); err != nil {
		t.Fatal(err)
	}
	if after, _ := a.host.Connection(b.ID()); after != before {
		t.Fatal("upgrade replaced inner connection")
	}
	reactions, unsub := na.pipeline.Subscribe()
	defer unsub()
	na.monitor.Notify(netmon.Route)
	for {
		select {
		case <-ctx.Done():
			t.Fatal("pipeline did not finish", ctx.Err())
		case reaction := <-reactions:
			if reaction.Stage == netmon.Complete && reaction.Event.Kind&netmon.Route != 0 {
				if !errors.Is(reaction.Err, errPhysicalRebind) {
					t.Fatal("claimed physical socket replacement worked", reaction.Err)
				}
				return
			}
		}
	}
}

func TestPrivateAndIsolatedDisablePublicConnectivity(t *testing.T) {
	for _, private := range []bool{false, true} {
		s, err := Start(context.Background(), Config{Key: swarmKey(t), Private: private, Isolated: !private, Logger: discard})
		if err != nil {
			t.Fatal(err)
		}
		if s.connectivity.client != nil || s.connectivity.network != nil || s.connectivity.homes != nil {
			t.Fatal("public connectivity enabled")
		}
		for _, p := range s.host.Protocols() {
			if p == disco.Protocol {
				t.Fatal("path protocol registered in private/isolated mode")
			}
		}
		s.Close()
	}
}
