package wss

import (
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	ma "github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Endpoint accepts exactly a single explicit /dns|dns4|dns6|ip4|ip6/.../tcp/
// <port>/tls/ws/p2p/<peer> (or /wss shorthand). It does no DNS resolution and
// grants no authorization. DNS names are validated by TLS when dialing.
func Endpoint(addr wire.Multiaddr) (string, wire.PeerID, error) {
	m, err := ma.NewMultiaddrBytes(addr.Bytes())
	if err != nil || addr.Opaque() || len(m) < 4 {
		return "", "", wire.ErrFormat
	}
	if m[1].Code() != ma.P_TCP || m[len(m)-1].Code() != ma.P_P2P {
		return "", "", wire.ErrFormat
	}
	if !(len(m) == 4 && m[2].Code() == ma.P_WSS || len(m) == 5 && m[2].Code() == ma.P_TLS && m[3].Code() == ma.P_WS) {
		return "", "", wire.ErrFormat
	}
	name := m[0].Value()
	switch m[0].Code() {
	case ma.P_IP4, ma.P_IP6:
		ip, err := netip.ParseAddr(name)
		if err != nil || ip.IsUnspecified() || ip.IsMulticast() {
			return "", "", wire.ErrFormat
		}
	case ma.P_DNS, ma.P_DNS4, ma.P_DNS6:
		if len(name) == 0 || len(name) > 253 || strings.ContainsAny(name, "/:@?#[]%\\\r\n\t ") {
			return "", "", wire.ErrFormat
		}
	default:
		return "", "", wire.ErrFormat
	}
	port, err := strconv.ParseUint(m[1].Value(), 10, 16)
	if err != nil || port == 0 {
		return "", "", wire.ErrFormat
	}
	id, err := wire.ParsePeerID(m[len(m)-1].Value())
	if err != nil {
		return "", "", err
	}
	u := url.URL{Scheme: "wss", Host: net.JoinHostPort(name, m[1].Value()), Path: "/"}
	return u.String(), id, nil
}
