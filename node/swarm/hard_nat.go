package swarm

import (
	"net/netip"

	"github.com/whyrusleeping/nu/p2p/disco"
	"github.com/whyrusleeping/nu/p2p/netcheck"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func (s *Swarm) hardNATConfig() *disco.HardConfig {
	if s.cfg.HardNAT == "off" || s.cfg.Private || s.cfg.Isolated {
		return nil
	}
	return &disco.HardConfig{
		Profile: func() (netcheck.Report, bool) { r, ok, _ := s.NATProfile(); return r, ok },
		Authorize: func(peer wire.PeerID, ip netip.Addr) bool {
			// Existing exact grants can explicitly opt a trusted peer's NAT IP
			// into the extension (including a lab/private network).
			for _, ep := range s.cfg.ProbeAllowed[peer] {
				if ep.Addr().Unmap() == ip {
					return true
				}
			}
			return hardPublicIP(ip)
		},
	}
}

// Do not let public-session assertions aim the extension at local, special-use,
// benchmark, documentation, multicast or reserved networks. No DNS is resolved.
func hardPublicIP(ip netip.Addr) bool {
	if !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
		return false
	}
	for _, text := range []string{"0.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4"} {
		if netip.MustParsePrefix(text).Contains(ip) {
			return false
		}
	}
	return true
}
