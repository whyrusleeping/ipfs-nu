package swarm

import (
	"context"
	"log/slog"
	"testing"

	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// swarm.dht=auto runs no DHT on a loopback-only listener; "client" runs one, and the
// connectivity owner's AutoNAT hook switches server mode on and off.
func TestDHTModeAndReachabilityHook(t *testing.T) {
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	base := Config{Key: key, Listen: []string{"/ip4/127.0.0.1/udp/0/quic-v1"}, Logger: slog.New(slog.DiscardHandler)}
	auto, err := Start(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer auto.Close()
	if auto.DHT() != nil || auto.DHTStatus().Mode != "off" {
		t.Fatal("auto mode started a DHT on a loopback-only listener")
	}

	cfg := base
	cfg.DHT = DHTConfig{Mode: "client", AllowPrivateAddresses: true}
	s, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.DHT() == nil || s.DHTStatus().Mode != "client" {
		t.Fatalf("client mode: %+v", s.DHTStatus())
	}
	s.dhtReachable(true)
	if s.DHTStatus().Mode != "server" || !hasProtocol(s, kad.Protocol) {
		t.Fatal("public verdict did not enable server mode")
	}
	s.dhtReachable(false)
	if s.DHTStatus().Mode != "client" || hasProtocol(s, kad.Protocol) {
		t.Fatal("lost reachability did not return to client mode")
	}
}

func hasProtocol(s *Swarm, p string) bool {
	for _, x := range s.host.Protocols() {
		if x == p {
			return true
		}
	}
	return false
}
