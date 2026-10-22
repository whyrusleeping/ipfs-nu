package swarm

import (
	"net/netip"
	"testing"

	"github.com/whyrusleeping/nu/node/repo"
)

func TestHardNATPolicyAndConfig(t *testing.T) {
	for _, text := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.1.1", "198.18.0.1", "192.0.2.1", "224.0.0.1", "240.0.0.1", "::1"} {
		if hardPublicIP(netip.MustParseAddr(text)) {
			t.Fatal("public policy admitted sensitive IP", text)
		}
	}
	if !hardPublicIP(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("public IPv4 policy")
	}
	var cfg Config
	if err := applyConnectivityConfig(&cfg, repo.SwarmConfig{HardNAT: "off"}); err != nil {
		t.Fatal(err)
	}
	if (&Swarm{cfg: cfg}).hardNATConfig() != nil {
		t.Fatal("off retained hard-NAT capability")
	}
	cfg.HardNAT = "auto"
	cfg.Private = true
	if (&Swarm{cfg: cfg}).hardNATConfig() != nil {
		t.Fatal("private mode retained hard-NAT capability")
	}
}
