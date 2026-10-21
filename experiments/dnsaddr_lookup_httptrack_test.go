package experiments_test

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/record"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	libquic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	ma "github.com/multiformats/go-multiaddr"
	madns "github.com/multiformats/go-multiaddr-dns"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/relay"
)

func TestDNSADDRLookupInterop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	directory, err := relay.NewLookup(relay.LookupConfig{Directory: lookup.DirectoryConfig{Domain: "peers.example.test"}, DNSListen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	resolver, err := madns.NewResolver(madns.WithDefaultResolver(&net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, directory.DNSAddr().String())
	}}))
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := libp2p.New(libp2p.NoTransports, libp2p.Transport(libquic.NewTransport), libp2p.ListenAddrStrings("/ip4/127.0.0.1/udp/0/quic-v1"), libp2p.DisableRelay(), libp2p.MultiaddrResolver(swarm.ResolverFromMaDNS{Resolver: resolver}))
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	nu, err := host.New(host.Config{Key: key, Listen: []string{"127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer nu.Close()
	const echo = "/nu/dnsaddr-interop/1"
	if err = nu.Handle(echo, 4, func(s *host.Stream) {
		_ = s.SetDeadline(time.Now().Add(time.Second))
		var b [4]byte
		if _, err := io.ReadFull(s, b[:]); err == nil {
			_, _ = s.Write(b[:])
		}
	}); err != nil {
		t.Fatal(err)
	}
	signed, err := lookup.SealPublishedRecord(key, 1, nu.Addrs(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = directory.Directory.Publish(ctx, nu.ID(), signed); err != nil {
		t.Fatal(err)
	}
	address := ma.StringCast("/dnsaddr/" + nu.ID().Base36() + ".peers.example.test/p2p/" + nu.ID().String())
	info, err := peer.AddrInfoFromP2pAddr(address)
	if err != nil {
		t.Fatal(err)
	}
	// Only dnsaddr is supplied: stock go-libp2p performs DNS resolution itself,
	// dials the returned Nu QUIC endpoint, and verifies the requested peer identity.
	if err = upstream.Connect(ctx, *info); err != nil {
		t.Fatal(err)
	}
	stream, err := upstream.NewStream(ctx, info.ID, protocol.ID(echo))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(time.Second))
	if _, err = stream.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	var got [4]byte
	if _, err = io.ReadFull(stream, got[:]); err != nil || string(got[:]) != "ping" {
		t.Fatal(string(got[:]), err)
	}
	// A standard go-libp2p signed peer record is also accepted by Nu's directory.
	upstreamRecord := &peer.PeerRecord{PeerID: upstream.ID(), Seq: 1, Addrs: upstream.Addrs()}
	envelope, err := record.Seal(upstreamRecord, upstream.Peerstore().PrivKey(upstream.ID()))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := envelope.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	upstreamID, err := wire.ParsePeerID(upstream.ID().String())
	if err != nil {
		t.Fatal(err)
	}
	if err = directory.Directory.Publish(ctx, upstreamID, raw); err != nil {
		t.Fatal(err)
	}
	source, err := lookup.NewDNS(lookup.DNSConfig{Domain: "peers.example.test", Servers: []string{directory.DNSAddr().String()}})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	if err = source.Lookup(ctx, upstreamID, func(a lookup.Address) bool { count++; return true }); err != nil || count == 0 {
		t.Fatal("upstream signed record lookup", count, err)
	}
}
