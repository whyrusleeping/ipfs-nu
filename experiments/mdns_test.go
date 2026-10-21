package experiments_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	libmdns "github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	libquic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
)

type foundPeer chan peer.AddrInfo

func (f foundPeer) HandlePeerFound(info peer.AddrInfo) {
	select {
	case f <- info:
	default:
	}
}
func TestMDNSInterop(t *testing.T) {
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Skip(err)
	}
	var iface *net.Interface
	var ip net.IP
	for _, i := range interfaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagMulticast == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			candidate, _, _ := net.ParseCIDR(a.String())
			if candidate.To4() != nil {
				iface = &i
				ip = candidate
				break
			}
		}
		if iface != nil {
			break
		}
	}
	if iface == nil {
		t.Skip("no LAN multicast interface")
	}
	upstream, err := libp2p.New(libp2p.NoTransports, libp2p.Transport(libquic.NewTransport), libp2p.ListenAddrStrings("/ip4/"+ip.String()+"/udp/0/quic-v1"), libp2p.DisableRelay())
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	nu, err := host.New(host.Config{Key: key, Listen: []string{net.JoinHostPort(ip.String(), "0")}})
	if err != nil {
		t.Fatal(err)
	}
	defer nu.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	cluster := "nu-interop-" + nu.ID().String()
	discovery, err := lookup.New(ctx, lookup.Config{MDNS: lookup.MDNSConfig{Peer: nu.ID(), Addresses: nu.Addrs, Interfaces: []net.Interface{*iface}, ClusterID: cluster}})
	if err != nil {
		t.Skip("multicast unavailable", err)
	}
	defer discovery.Close()
	found := make(foundPeer, 8)
	upstreamMDNS := libmdns.NewMdnsService(upstream, strings.TrimSuffix(lookup.ServiceName(cluster), ".local."), found)
	if err := upstreamMDNS.Start(); err != nil {
		t.Skip("go-libp2p multicast unavailable", err)
	}
	defer upstreamMDNS.Close()
	remote, err := wire.ParsePeerID(upstream.ID().String())
	if err != nil {
		t.Fatal(err)
	}
	seenNu, seenUpstream := false, false
	results := discovery.Lookup(ctx, remote)
	for !seenNu || !seenUpstream {
		select {
		case info := <-found:
			if info.ID.String() == nu.ID().String() {
				seenNu = true
			}
		case result, ok := <-results:
			if !ok {
				results = nil
			} else if result.Err == nil && result.Address.Peer == remote {
				seenUpstream = true
			}
		case <-ctx.Done():
			t.Fatalf("mDNS interop on %s: Nu seen=%v go-libp2p seen=%v", iface.Name, seenNu, seenUpstream)
		}
	}
	upstream.SetStreamHandler("/test/mdns-session/1", func(s network.Stream) {
		defer s.Close()
		_ = s.SetDeadline(time.Now().Add(time.Second))
		b, err := wire.ReadMessage(s, 16)
		if err == nil {
			_ = wire.WriteMessage(s, b, 16)
		}
	})
	sessions, err := host.NewSessions(nu, host.SessionConfig{Lookup: discovery, Cache: discovery.Cache()})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	stream, err := sessions.OpenStream(ctx, remote, "/test/mdns-session/1")
	if err != nil {
		t.Fatal("LAN session connect", err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(time.Second))
	if err := wire.WriteMessage(stream, []byte("LAN transfer"), 16); err != nil {
		t.Fatal(err)
	}
	response, err := wire.ReadMessage(stream, 16)
	if err != nil || string(response) != "LAN transfer" {
		t.Fatal("LAN session transfer", err)
	}
}
