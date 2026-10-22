package host

import (
	"net"

	ma "github.com/multiformats/go-multiaddr"

	quic "github.com/quic-go/quic-go"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// PacketConnPolicy is an optional extension implemented by virtual-address
// sockets. Host stays transport-pluggable: it does not import magicsock. Config
// policy is applied before incoming/outgoing QUIC setup; VerifyPeerAddress runs
// on the authenticated TLS identity before admitting a connection or streams.
// Policies must leave physical-address connections wire-compatible with libp2p.
type PacketConnPolicy interface {
	QUICConfigForPeer(remote net.Addr, base *quic.Config) *quic.Config
	VerifyPeerAddress(remote net.Addr, peer wire.PeerID) error
}

// IsLocalOnlyMultiaddr checks literal IP components against the socket policy,
// including relay prefixes. It does not resolve DNS or authorize a dial.
func (h *Host) IsLocalOnlyMultiaddr(a wire.Multiaddr) bool {
	m, err := ma.NewMultiaddrBytes(a.Bytes())
	if err != nil {
		return false
	}
	for _, part := range m {
		if part.Code() == ma.P_IP4 || part.Code() == ma.P_IP6 {
			if h.IsLocalOnlyAddress(&net.UDPAddr{IP: net.ParseIP(part.Value()), Port: 1}) {
				return true
			}
		}
	}
	return false
}

// IsLocalOnlyAddress prevents virtual transport routing keys from escaping into
// discovery/identify observations or the sessions' verified-address cache.
func (h *Host) IsLocalOnlyAddress(addr net.Addr) bool {
	for _, ep := range h.endpoints {
		if policy, ok := ep.socket.(interface{ IsLocalOnlyAddress(net.Addr) bool }); ok && policy.IsLocalOnlyAddress(addr) {
			return true
		}
	}
	return false
}
