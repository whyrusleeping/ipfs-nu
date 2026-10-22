package homerelay

import (
	"testing"

	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestQUICOnlyEndpointAdmission(t *testing.T) {
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	peer := key.Public().PeerID()
	parse := func(prefix string) wire.Multiaddr {
		t.Helper()
		addr, err := wire.ParseMultiaddr(prefix+"/p2p/"+peer.String(), 1024)
		if err != nil {
			t.Fatal(err)
		}
		return addr
	}
	var endpoints Endpoints
	admitted := parse("/ip4/127.0.0.1/udp/443/quic-v1")
	if err := endpoints.Add(admitted); err != nil || !endpoints.Contains(admitted) {
		t.Fatal("QUIC endpoint not admitted", err)
	}
	for _, prefix := range []string{
		"/ip4/127.0.0.1/udp/444/quic-v1",
		"/ip4/127.0.0.1/tcp/443",
		"/dns/relay.example/tcp/443/tls/ws",
	} {
		addr := parse(prefix)
		if endpoints.Add(addr) == nil || endpoints.Contains(addr) {
			t.Fatal("admitted additional or non-QUIC endpoint", addr)
		}
	}
	if _, err := circuit.Address(parse("/dns/relay.example/tcp/443/tls/ws"), peer); err == nil {
		t.Fatal("accepted non-QUIC circuit address")
	}
	if endpoints.QUIC.String() != admitted.String() {
		t.Fatal("failed admission changed the endpoint")
	}
}
