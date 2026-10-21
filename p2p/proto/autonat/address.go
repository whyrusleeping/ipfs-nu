package autonat

import (
	"net/netip"
	"strconv"

	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// endpoint only accepts literal direct QUIC, with an optional matching /p2p.
// No DNS resolution, relay, extra transports or address fallback is allowed.
func endpoint(a wire.Multiaddr, id wire.PeerID, allowPrivate bool) (wire.Multiaddr, netip.Addr, error) {
	normalized, err := lookup.NormalizeAddress(id, a)
	if err != nil {
		return wire.Multiaddr{}, netip.Addr{}, err
	}
	m, err := ma.NewMultiaddrBytes(normalized.Bytes())
	if err != nil {
		return wire.Multiaddr{}, netip.Addr{}, err
	}
	if len(m) != 4 || (m[0].Code() != ma.P_IP4 && m[0].Code() != ma.P_IP6) || m[1].Code() != ma.P_UDP || m[2].Code() != ma.P_QUIC_V1 || m[3].Code() != ma.P_P2P {
		return wire.Multiaddr{}, netip.Addr{}, wire.ErrFormat
	}
	ip, err := netip.ParseAddr(m[0].Value())
	if err != nil {
		return wire.Multiaddr{}, netip.Addr{}, err
	}
	port, err := strconv.ParseUint(m[1].Value(), 10, 16)
	if err != nil || port == 0 || ip.Is4In6() || ip.IsUnspecified() || ip.IsMulticast() || (!allowPrivate && !manet.IsPublicAddr(m)) {
		return wire.Multiaddr{}, netip.Addr{}, wire.ErrFormat
	}
	return normalized, ip.Unmap(), nil
}
