package proto_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/magicsock"
	nuproto "github.com/whyrusleeping/nu/p2p/proto"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestAddressProviderSignedPushAndVirtualExclusion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	makeHost := func(virtual bool) *host.Host {
		key, err := wire.GenerateNodeKey()
		if err != nil {
			t.Fatal(err)
		}
		cfg := host.Config{Key: key}
		if virtual {
			udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			socket, err := magicsock.New(magicsock.Config{UDP: udp})
			if err != nil {
				udp.Close()
				t.Fatal(err)
			}
			cfg.PacketConns = []net.PacketConn{socket}
		}
		h, err := host.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { h.Close() })
		return h
	}
	a, b, relay := makeHost(false), makeHost(true), makeHost(false)
	var mu sync.Mutex
	addresses := b.Addrs()
	sender, err := nuproto.New(b, nuproto.Config{Addresses: func() []wire.Multiaddr {
		mu.Lock()
		defer mu.Unlock()
		return append([]wire.Multiaddr(nil), addresses...)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	records := make(chan wire.PeerRecord, 16)
	receiver, err := nuproto.New(a, nuproto.Config{OnPeerRecord: func(r wire.PeerRecord) {
		select {
		case records <- r:
		default:
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	addr, err := wire.ParseMultiaddr(b.Addrs()[0].String()+"/p2p/"+b.ID().String(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Dial(ctx, addr); err != nil {
		t.Fatal(err)
	}
	info, err := receiver.WaitPeer(ctx, b.ID(), nil)
	if err != nil || info.Certified == nil {
		t.Fatal("initial record", err)
	}
	initial := info.Certified.Seq
	circuit, err := wire.ParseMultiaddr(relay.Addrs()[0].String()+"/p2p/"+relay.ID().String()+"/p2p-circuit/p2p/"+b.ID().String(), 2048)
	if err != nil {
		t.Fatal(err)
	}
	virtual, err := wire.ParseMultiaddr("/ip4/240.0.0.1/udp/1/quic-v1", 1024)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	addresses = []wire.Multiaddr{circuit, virtual}
	mu.Unlock()
	sender.AddressesChanged()
	info, err = receiver.WaitPeer(ctx, b.ID(), func(p nuproto.PeerInfo) bool {
		return p.Certified != nil && p.Certified.Seq > initial && len(p.Certified.Addresses) == 1 && p.Certified.Addresses[0].String() == circuit.String()
	})
	if err != nil {
		t.Fatal("signed relay update did not arrive", err)
	}
	if len(info.Message.ListenAddrs) != 1 || info.Message.ListenAddrs[0].String() != circuit.String() {
		t.Fatal("provider addresses differed from signed record, or virtual escaped", info)
	}
	for {
		select {
		case record := <-records:
			if record.Seq == info.Certified.Seq {
				if record.PeerID != b.ID() {
					t.Fatal("callback identity")
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("authenticated record callback missing", ctx.Err())
		}
	}
}
