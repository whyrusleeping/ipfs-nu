package swarm

import (
	"net/netip"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// cgnat is the carrier-grade NAT shared range (RFC 6598), also used by tailnets
// (Tailscale). Go's IsPrivate does not cover it, but it is reachable only inside the
// carrier/tailnet, so it is not public.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// IsPublicIP reports whether ip may be published as a globally reachable address: global
// unicast and none of private (RFC 1918/4193), CGNAT, loopback, link-local or
// unspecified. Explicit user links and LAN hints may still carry the excluded ranges.
func IsPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !cgnat.Contains(ip) && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified()
}

// publicationAddrs derives identify's public signed snapshot from the SAME live
// provider used by user share links. Explicit share links may carry LAN hints;
// public signed records must not disclose LAN/loopback literals. Circuit prefixes
// are deliberately configured/known relay endpoints, not private disco candidates.
func (s *Swarm) publicationAddrs() []wire.Multiaddr {
	var out []wire.Multiaddr
	for _, a := range s.Addrs() {
		if host.IsCircuitAddress(a) {
			out = append(out, a)
			continue
		}
		ep, ok := literalEndpoint(a)
		if !ok {
			continue
		}
		if IsPublicIP(ep.Addr()) && !s.host.IsLocalOnlyMultiaddr(a) {
			out = append(out, a)
		}
	}
	return out
}
