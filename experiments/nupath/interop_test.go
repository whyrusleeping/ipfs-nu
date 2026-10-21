package nupath

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
	libclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/magicsock"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/proto/qpath"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Real standard-peer security multistream negotiation, not a fake `na` server.
func TestQpathDeclined(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	rh, rk := newHost(t)
	srv, err := circuit.NewServer(rh, rk, circuit.ServerConfig{})
	check(t, err)
	defer srv.Close()
	upstream, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/udp/0/quic-v1"))
	check(t, err)
	defer upstream.Close()
	relayID, err := peer.Decode(rh.ID().String())
	check(t, err)
	_, err = libclient.Reserve(ctx, upstream, peer.AddrInfo{ID: relayID, Addrs: []ma.Multiaddr{ma.StringCast(rh.Addrs()[0].String())}})
	check(t, err)
	raw, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	check(t, err)
	socket, err := magicsock.New(magicsock.Config{UDP: raw})
	check(t, err)
	defer socket.Close()
	key, err := wire.GenerateNodeKey()
	check(t, err)
	source, err := host.New(host.Config{Key: key, PacketConns: []net.PacketConn{socket}})
	check(t, err)
	defer source.Close()
	standardAddr, err := wire.ParseMultiaddr(upstream.Addrs()[0].String()+"/p2p/"+upstream.ID().String(), 1024)
	check(t, err)
	_, err = source.Dial(ctx, standardAddr)
	check(t, err) // Real-address pass-through on magicsock.
	addr, err := wire.ParseMultiaddr(rh.Addrs()[0].String()+"/p2p/"+rh.ID().String(), 1024)
	check(t, err)
	_, err = source.Dial(ctx, addr)
	check(t, err)
	client, err := circuit.NewClient(source, circuit.ClientConfig{})
	check(t, err)
	defer client.Close()
	target, err := wire.ParsePeerID(upstream.ID().String())
	check(t, err)
	_, _, err = qpath.Dial(ctx, socket, client, rh.ID(), target)
	if !errors.Is(err, wire.ErrNotSupported) {
		t.Fatalf("expected clean security-protocol na, got %v", err)
	}
	if socket.Stats().Peers != 0 {
		t.Fatal("declined qpath allocated virtual path")
	}
	t.Log("go-libp2p v0.50.0 declined /nu/qpath/1 with na; standard secure fallback not attempted")
}
