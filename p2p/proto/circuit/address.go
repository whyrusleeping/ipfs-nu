package circuit

import (
	"net/netip"
	"strconv"

	ma "github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/nu/p2p/wire"
)

func relayAddress(a wire.Multiaddr, id wire.PeerID) error {
	if a.Opaque() {
		return wire.ErrFormat
	}
	m, err := ma.NewMultiaddrBytes(a.Bytes())
	if err != nil {
		return err
	}
	if len(m) != 4 || (m[0].Code() != ma.P_IP4 && m[0].Code() != ma.P_IP6) || m[1].Code() != ma.P_UDP || m[2].Code() != ma.P_QUIC_V1 || m[3].Code() != ma.P_P2P {
		return wire.ErrFormat
	}
	ip, err := netip.ParseAddr(m[0].Value())
	if err != nil || ip.IsUnspecified() || ip.IsMulticast() {
		return wire.ErrFormat
	}
	port, err := strconv.ParseUint(m[1].Value(), 10, 16)
	if err != nil || port == 0 {
		return wire.ErrFormat
	}
	peer, err := wire.ParsePeerID(m[3].Value())
	if err != nil || peer != id {
		return wire.ErrFormat
	}
	return nil
}
