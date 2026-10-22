package swarm

import (
	"testing"

	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/disco"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
)

func TestConnectivityConfigAndRuntimePrivacy(t *testing.T) {
	id := swarmKey(t).Public().PeerID()
	valid := repo.SwarmConfig{Mode: "private", Relays: []string{"/ip4/127.0.0.1/udp/4000/quic-v1/p2p/" + id.String()}, Peers: []string{"/ip4/127.0.0.1/udp/4001/quic-v1/p2p/" + id.String()}, ProbeAllow: []string{id.String() + "@127.0.0.1:4001"}, PortmapGateway: "192.168.1.1"}
	var cfg Config
	if err := applyConnectivityConfig(&cfg, valid); err != nil {
		t.Fatal(err)
	}
	if !cfg.Private || len(cfg.Relays) != 1 || len(cfg.Peers[id]) != 1 || len(cfg.ProbeAllowed[id]) != 1 || cfg.PortMapping == nil {
		t.Fatal("config incomplete", cfg)
	}
	for _, bad := range []repo.SwarmConfig{{Mode: "typo"}, {Relays: []string{"/ip4/240.0.0.1/udp/1/quic-v1/p2p/" + id.String()}}, {ProbeAllow: []string{id.String() + "@10.0.0.0/8"}}, {PortmapGateway: "gateway.example"}} {
		var out Config
		if err := applyConnectivityConfig(&out, bad); err == nil {
			t.Fatal("accepted malformed connectivity config", bad)
		}
	}
	s := testSwarm(t, swarmKey(t))
	s.DisablePublic()
	s.DisablePublic()
	status := s.ConnectivityStatus()
	if status.Reachability != "disabled" || status.Home != "" || status.PhysicalRebind {
		t.Fatal(status)
	}
	for _, p := range s.host.Protocols() {
		if p == disco.Protocol || p == circuit.StopProtocol {
			t.Fatal("public handler survived runtime privacy transition", p)
		}
	}
}
