package swarm

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/magicsock"
	"github.com/whyrusleeping/nu/p2p/portmap"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/p2p/wss"
)

func applyConnectivityConfig(out *Config, in repo.SwarmConfig) error {
	switch in.Mode {
	case "", "public":
	case "private":
		out.Private = true
	case "isolated":
		out.Isolated = true
	default:
		return fmt.Errorf("swarm.mode: expected public, private or isolated, got %q", in.Mode)
	}
	if len(in.Relays) > 32 || len(in.Peers) > 128 || len(in.ProbeAllow) > 4096 {
		return fmt.Errorf("swarm: too many configured relay, peer or probe endpoints")
	}
	for _, text := range in.Relays {
		addr, err := wire.ParseMultiaddr(text, 1024)
		if err != nil {
			return fmt.Errorf("swarm.relays: %w", err)
		}
		if _, err := circuit.RelayPeer(addr); err != nil {
			return fmt.Errorf("swarm.relays: QUIC or WSS address with /p2p/<relay> required: %w", err)
		}
		if _, _, err := wss.Endpoint(addr); err != nil {
			if ep, ok := literalEndpoint(addr); !ok || magicsock.IsVirtual(ep) {
				return fmt.Errorf("swarm.relays: local virtual addresses cannot be configured")
			}
		}
		out.Relays = append(out.Relays, addr)
	}
	if err := applyWSSConfig(out, in); err != nil {
		return err
	}
	out.Peers = make(map[wire.PeerID][]wire.Multiaddr)
	for _, text := range in.Peers {
		addr, err := wire.ParseMultiaddr(text, 1024)
		if err != nil {
			return fmt.Errorf("swarm.peers: %w", err)
		}
		id, err := lookup.AddressPeer(addr)
		if err != nil {
			return fmt.Errorf("swarm.peers: /p2p/<peer> required: %w", err)
		}
		if ep, ok := literalEndpoint(addr); !ok || magicsock.IsVirtual(ep) {
			return fmt.Errorf("swarm.peers: literal physical endpoint required")
		}
		if len(out.Peers[id]) >= 32 {
			return fmt.Errorf("swarm.peers: too many addresses for one peer")
		}
		out.Peers[id] = append(out.Peers[id], addr)
	}
	out.ProbeAllowed = make(map[wire.PeerID][]netip.AddrPort)
	for _, text := range in.ProbeAllow {
		peer, endpoint, ok := strings.Cut(text, "@")
		id, err := wire.ParsePeerID(peer)
		if !ok || err != nil {
			return fmt.Errorf("swarm.probe_allow: peerID@IP:port required")
		}
		addr, err := netip.ParseAddrPort(endpoint)
		if err != nil || addr.Port() == 0 || addr.Addr().IsUnspecified() || addr.Addr().IsMulticast() || addr.Addr().IsLinkLocalUnicast() || magicsock.IsVirtual(addr) {
			return fmt.Errorf("swarm.probe_allow: valid exact physical endpoint required")
		}
		if len(out.ProbeAllowed[id]) >= 32 || len(out.ProbeAllowed) >= 128 && len(out.ProbeAllowed[id]) == 0 {
			return fmt.Errorf("swarm.probe_allow: at most 128 peers and 32 endpoints per peer")
		}
		out.ProbeAllowed[id] = append(out.ProbeAllowed[id], addr)
	}
	if len(in.PeerRelayAllow) > 64 {
		return fmt.Errorf("swarm.peer_relay_allow: at most 64 peers")
	}
	for _, text := range in.PeerRelayAllow {
		id, err := wire.ParsePeerID(text)
		if err != nil {
			return fmt.Errorf("swarm.peer_relay_allow: %w", err)
		}
		out.PeerRelayAllow = append(out.PeerRelayAllow, id)
	}
	if in.PortmapGateway != "" {
		gateway, err := netip.ParseAddr(in.PortmapGateway)
		if err != nil || gateway.IsUnspecified() || gateway.IsMulticast() {
			return fmt.Errorf("swarm.portmap_gateway: a literal gateway IP is required")
		}
		out.PortMapping = &portmap.Config{Gateway: gateway}
	}
	return nil
}
