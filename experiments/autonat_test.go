package experiments_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	libautonat "github.com/libp2p/go-libp2p/p2p/protocol/autonatv2"
	libquic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	ma "github.com/multiformats/go-multiaddr"
	nuproto "github.com/whyrusleeping/nu/p2p/proto"
	"github.com/whyrusleeping/nu/p2p/proto/autonat"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Exercise the actual upstream AutoNAT v2 implementation, not a protobuf stub.
// Its private-address override and Nu's equivalent are only for this local lab.
func TestAutoNATv2(t *testing.T) {
	nu, upstream := endpoints(t)
	core, err := nuproto.New(nu, nuproto.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	nat, err := autonat.New(nu, autonat.Config{Server: true, AllowPrivateAddresses: true})
	if err != nil {
		t.Fatal(err)
	}
	defer nat.Close()
	dialer, err := libp2p.New(libp2p.NoTransports, libp2p.Transport(libquic.NewTransport), libp2p.ListenAddrStrings("/ip4/127.0.0.1/udp/0/quic-v1"), libp2p.DisableRelay())
	if err != nil {
		t.Fatal(err)
	}
	remote, err := libautonat.New(dialer, libautonat.AllowPrivateAddrs)
	if err != nil {
		dialer.Close()
		t.Fatal(err)
	}
	if err := remote.Start(upstream); err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	connectPair(t, nu, upstream)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := wire.ParsePeerID(upstream.ID().String())
	if err != nil {
		t.Fatal(err)
	}
	result, err := nat.Check(ctx, id, []autonat.Request{{Address: nu.Addrs()[0], SendDialData: true}})
	if err != nil || result.Reachability != autonat.Reachable {
		t.Fatal("Nu client -> go-libp2p server", result, err)
	}
	// A second loopback IP forces the real upstream client to pay Nu's dial
	// data challenge before Nu can send any handshake to that destination.
	if err := upstream.Network().Listen(ma.StringCast("/ip4/127.0.0.2/udp/0/quic-v1")); err != nil {
		t.Fatal(err)
	}
	var candidate ma.Multiaddr
	for _, a := range upstream.Network().ListenAddresses() {
		if strings.HasPrefix(a.String(), "/ip4/127.0.0.2/") {
			candidate = a
			break
		}
	}
	if candidate == nil {
		t.Fatal("missing alternate-IP listener")
	}
	// Upstream learns eligible servers asynchronously from identify/events.
	for {
		result, err := remote.GetReachability(ctx, []libautonat.Request{{Addr: candidate, SendDialData: true}})
		if errors.Is(err, libautonat.ErrNoPeers) {
			select {
			case <-time.After(10 * time.Millisecond):
				continue
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		if err != nil || result.Reachability != network.ReachabilityPublic {
			t.Fatal("go-libp2p client -> Nu server", result, err)
		}
		break
	}
}
