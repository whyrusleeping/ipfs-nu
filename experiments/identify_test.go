package experiments_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	libping "github.com/libp2p/go-libp2p/p2p/protocol/ping"
	"github.com/whyrusleeping/nu/p2p/host"
	nuproto "github.com/whyrusleeping/nu/p2p/proto"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestIdentifyPushPing(t *testing.T) {
	nu, upstream := endpoints(t)
	events, err := upstream.EventBus().Subscribe([]interface{}{new(event.EvtPeerIdentificationCompleted), new(event.EvtPeerProtocolsUpdated)})
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	service, err := nuproto.New(nu, nuproto.Config{AgentVersion: "interop/1"})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	connectPair(t, nu, upstream)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	remote, err := wire.ParsePeerID(upstream.ID().String())
	if err != nil {
		t.Fatal(err)
	}
	info, err := service.WaitPeer(ctx, remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(info.Message.Protocols, nuproto.IdentifyID) || !slices.Contains(info.Message.Protocols, nuproto.PingID) || info.Certified == nil || info.Message.ObservedAddr == nil {
		t.Fatal("incomplete remote identify", info)
	}
	if info.Message.ObservedAddr.String() != nu.Addrs()[0].String() {
		t.Fatal("Nu's observed address", info.Message.ObservedAddr)
	}
	observations := service.Observations()
	if len(observations) != 1 || observations[0].Address.String() != nu.Addrs()[0].String() || observations[0].ObserverEndpoint == "" {
		t.Fatal("observer endpoint aggregation", observations)
	}
	nuID, err := peer.Decode(nu.ID().String())
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case e := <-events.Out():
			identified, ok := e.(event.EvtPeerIdentificationCompleted)
			if !ok || identified.Peer != nuID {
				continue
			}
			if identified.AgentVersion != "interop/1" || !slices.Contains(identified.Protocols, protocol.ID(nuproto.PingID)) || identified.SignedPeerRecord == nil || identified.ObservedAddr == nil || identified.ObservedAddr.String() != upstream.Addrs()[0].String() {
				t.Fatal("upstream identify", identified)
			}
			goto identified
		case <-ctx.Done():
			t.Fatal("waiting for upstream identify", ctx.Err())
		}
	}
identified:
	const nuAdded = "/nu/pushed/1"
	if err := nu.Handle(nuAdded, 1, func(s *host.Stream) {}); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case e := <-events.Out():
			updated, ok := e.(event.EvtPeerProtocolsUpdated)
			if ok && updated.Peer == nuID && slices.Contains(updated.Added, protocol.ID(nuAdded)) {
				goto pushed
			}
		case <-ctx.Done():
			t.Fatal("waiting for Nu push", ctx.Err())
		}
	}
pushed:
	const remoteAdded = protocol.ID("/upstream/pushed/1")
	upstream.SetStreamHandler(remoteAdded, func(s network.Stream) { _ = s.Close() })
	if _, err := service.WaitPeer(ctx, remote, func(info nuproto.PeerInfo) bool { return slices.Contains(info.Message.Protocols, string(remoteAdded)) }); err != nil {
		t.Fatal("upstream push", err)
	}
	if rtt, err := service.Ping(ctx, remote); err != nil || rtt <= 0 {
		t.Fatal("Nu ping", rtt, err)
	}
	pingctx, stop := context.WithCancel(ctx)
	defer stop()
	select {
	case result := <-libping.Ping(pingctx, upstream, nuID):
		if result.Error != nil || result.RTT <= 0 {
			t.Fatal("upstream ping", result)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
