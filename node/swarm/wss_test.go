package swarm

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"testing"

	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/homerelay"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func TestWSSRelayPolicyAndRemoteAdmission(t *testing.T) {
	id := swarmKey(t).Public().PeerID()
	addr := func(name string) wire.Multiaddr {
		a, err := wire.ParseMultiaddr("/dns/"+name+"/tcp/443/tls/ws/p2p/"+id.String(), 1024)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	admitted, remote := addr("relay.example"), addr("untrusted.example")
	cfg := Config{}
	pin := sha256.Sum256([]byte("leaf"))
	in := repo.SwarmConfig{Relays: []string{admitted.String()}, Proxy: "http://user:secret@proxy.example:8080", RelayCertPins: []string{fmt.Sprintf("%s@%x", id, pin)}}
	if err := applyConnectivityConfig(&cfg, in); err != nil {
		t.Fatal(err)
	}
	opts := cfg.WSS.options(id)
	if opts.Proxy.Host != "proxy.example:8080" || opts.TLSConfig.InsecureSkipVerify {
		t.Fatal("proxy/TLS policy")
	}
	for _, tc := range []struct {
		raw   string
		valid bool
	}{{"leaf", true}, {"interceptor", false}} {
		err := opts.TLSConfig.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: []byte(tc.raw)}}})
		if (err == nil) != tc.valid {
			t.Fatal("pin verification", err)
		}
	}
	for _, bad := range []repo.SwarmConfig{{Proxy: "https://proxy.example"}, {Proxy: "http://proxy.example/path"}, {RelayCertPins: in.RelayCertPins}, {Relays: in.Relays, RelayCertPins: []string{id.String() + "@short"}}} {
		if applyConnectivityConfig(&Config{}, bad) == nil {
			t.Fatalf("accepted bad WSS policy: %+v", bad)
		}
	}
	// Signed records are routing hints, not authority to add a WSS alternative.
	s := testSwarm(t, swarmKey(t))
	c := s.connectivity
	peer := swarmKey(t).Public().PeerID()
	hint, _ := circuit.Address(admitted, peer)
	other, _ := circuit.Address(remote, peer)
	if err := c.admitHints(peer, []wire.Multiaddr{hint}); err != nil {
		t.Fatal(err)
	}
	c.learn(wire.PeerRecord{PeerID: peer, Seq: 1, Addresses: []wire.Multiaddr{other, hint}})
	p, err := c.peer(peer)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	routes := append([]wire.Multiaddr(nil), p.routes...)
	endpoints := c.known[id]
	c.mu.Unlock()
	if len(routes) != 1 || routes[0].String() != hint.String() || endpoints.Contains(remote) {
		t.Fatal("remote record extended relay authorization")
	}
	if _, _, err := c.authorized(other, peer); err == nil {
		t.Fatal("remote WSS endpoint authorized")
	}
	if err := c.admitHints(peer, []wire.Multiaddr{other}); err == nil {
		t.Fatal("second WSS endpoint admitted")
	}
	var bounded homerelay.Endpoints
	if err := bounded.Add(admitted); err != nil {
		t.Fatal(err)
	}
	udp, _ := wire.ParseMultiaddr("/ip4/127.0.0.1/udp/4000/quic-v1/p2p/"+id.String(), 1024)
	if err := bounded.Add(udp); err != nil {
		t.Fatal(err)
	}
}
