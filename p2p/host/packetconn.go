package host

import (
	"net"

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
