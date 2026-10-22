package circuit

import (
	"net/netip"
	"strconv"

	ma "github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/p2p/wss"
)

// RelayPeer validates an explicit literal QUIC-v1 or TLS WebSocket endpoint.
// Validation is not authorization; remote hints cannot extend admitted relays.
func RelayPeer(a wire.Multiaddr) (wire.PeerID, error) {
	if _, id, err := wss.Endpoint(a); err == nil {
		return id, nil
	}
	m, err := ma.NewMultiaddrBytes(a.Bytes())
	if err != nil || len(m) != 4 || m[3].Code() != ma.P_P2P {
		return "", wire.ErrFormat
	}
	id, err := wire.ParsePeerID(m[3].Value())
	if err != nil {
		return "", err
	}
	return id, relayAddress(a, id)
}

// Address is a public circuit hint, not a local qpath virtual address.
func Address(relay wire.Multiaddr, peer wire.PeerID) (wire.Multiaddr, error) {
	if _, err := RelayPeer(relay); err != nil {
		return wire.Multiaddr{}, err
	}
	if _, err := wire.PeerIDFromBytes(peer.Bytes()); err != nil {
		return wire.Multiaddr{}, err
	}
	return wire.ParseMultiaddr(relay.String()+"/p2p-circuit/p2p/"+peer.String(), 2048)
}

// ParseAddress accepts exactly one supported relay hop and a bound destination.
// Callers still apply relay and destination policy before opening any sockets.
func ParseAddress(a wire.Multiaddr) (relay wire.Multiaddr, relayPeer, peer wire.PeerID, err error) {
	m, err := ma.NewMultiaddrBytes(a.Bytes())
	if a.Opaque() || err != nil || (len(m) != 6 && len(m) != 7) || m[len(m)-2].Code() != ma.P_CIRCUIT || m[len(m)-1].Code() != ma.P_P2P {
		return relay, "", "", wire.ErrFormat
	}
	peer, err = wire.ParsePeerID(m[len(m)-1].Value())
	if err != nil {
		return relay, "", "", err
	}
	relay, err = wire.MultiaddrFromBytes(m[:len(m)-2].Bytes(), 2048)
	if err != nil {
		return relay, "", "", err
	}
	relayPeer, err = RelayPeer(relay)
	return relay, relayPeer, peer, err
}

func relayAddress(a wire.Multiaddr, id wire.PeerID) error {
	if _, peer, err := wss.Endpoint(a); err == nil {
		if peer != id {
			return wire.ErrFormat
		}
		return nil
	}
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
