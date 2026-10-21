package disco

import (
	"net"
	"net/netip"
	"sort"
	"time"

	"github.com/whyrusleeping/nu/p2p/portmap"
)

// Gather enumerates usable addresses on all up, non-loopback interfaces for the
// shared socket's port. Call only if that socket actually binds those addresses
// (usually wildcard); filter by socket family before advertising. IPv6 link-local
// addresses need receiver-local zone translation and are intentionally not v1.
// Results are deterministic, bounded and expiring; add observed/mapped candidates
// explicitly using the same socket, never observations of a different socket.
func Gather(port uint16, now time.Time) ([]Candidate, error) {
	if port == 0 {
		return nil, net.InvalidAddrError("disco: zero socket port")
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	seen := make(map[netip.AddrPort]bool)
	var out []Candidate
	for _, i := range interfaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			prefix, err := netip.ParsePrefix(a.String())
			if err != nil {
				continue
			}
			ip := prefix.Addr().Unmap()
			addr := netip.AddrPortFrom(ip, port)
			if !validEndpoint(addr) || seen[addr] {
				continue
			}
			seen[addr] = true
			kind := LAN
			if ip.Is6() && ip.IsGlobalUnicast() && !ip.IsPrivate() {
				kind = IPv6
			} else if !ip.IsPrivate() {
				kind = Reflexive
			}
			out = append(out, Candidate{kind, addr, now.Add(10 * time.Minute)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Addr.Compare(out[j].Addr) < 0
	})
	if len(out) > MaxCandidates {
		out = out[:MaxCandidates]
	}
	return out, nil
}

// FromMapping enforces lease expiry and PCP PEER's exact remote restriction.
// remote is the endpoint of the intended peer, not its relay. Mappings remain
// tentative and still require the receiver's destination policy and a round trip.
func FromMapping(m portmap.Mapping, remote netip.AddrPort, now time.Time) (Candidate, bool) {
	if !validEndpoint(m.External) || !now.Before(m.Expires) || (m.Remote.IsValid() && m.Remote != remote) {
		return Candidate{}, false
	}
	return Candidate{Mapped, m.External, m.Expires}, true
}
