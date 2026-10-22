package swarm

import (
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

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
		ip := ep.Addr().Unmap()
		if ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !s.host.IsLocalOnlyMultiaddr(a) {
			out = append(out, a)
		}
	}
	return out
}
