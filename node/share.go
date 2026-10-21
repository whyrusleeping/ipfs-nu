package node

import (
	"context"
	"net"
	"strings"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/node/link"
	"github.com/whyrusleeping/nu/node/target"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Net is what a running libp2p host tells the node about its reachability. The daemon
// sets Local.Net when it starts a host; until then (and in daemonless commands) it is nil
// and the node derives its addresses from the swarm listen config.
type Net interface {
	// Addrs are the multiaddrs the host advertises (no /p2p/ suffix).
	Addrs() []wire.Multiaddr
	// Peers counts connected peers.
	Peers() int
	// Connected lists the connected peers.
	Connected() []wire.PeerID
}

// MaxLinkAddrs bounds the address hints a share link carries: enough for a LAN and a
// public address each in IPv4 and IPv6, short enough to paste.
const MaxLinkAddrs = 3

// PeerID is the node's libp2p identity.
func (n *Local) PeerID() wire.PeerID { return n.Key.Public().PeerID() }

// Addrs returns the addresses this node would put in a share link: the host's when one
// runs, else the swarm listen config with unspecified addresses (0.0.0.0, ::) expanded over
// the machine's non-loopback, non-link-local interface addresses. At most MaxLinkAddrs,
// in listen-config order (IPv4 before IPv6 by default).
func (n *Local) Addrs() []wire.Multiaddr {
	var addrs []wire.Multiaddr
	if n.Net != nil {
		addrs = n.Net.Addrs()
	} else {
		cfg, err := n.Repo.Config()
		if err == nil {
			addrs = expandListen(cfg.Swarm.Listen, interfaceIPs)
		}
	}
	if len(addrs) > MaxLinkAddrs {
		addrs = addrs[:MaxLinkAddrs]
	}
	return addrs
}

// expandListen turns listen multiaddrs into advertisable ones: loopback and link-local
// addresses are dropped, unspecified addresses are replaced by each interface address of
// the same family, concrete addresses are kept. Malformed entries are skipped.
func expandListen(listen []string, ips func() []net.IP) []wire.Multiaddr {
	var out []wire.Multiaddr
	var local []net.IP
	add := func(s string) {
		a, err := wire.ParseMultiaddr(s, link.MaxAddrLen)
		if err == nil && !a.Opaque() {
			out = append(out, a)
		}
	}
	for _, s := range listen {
		parts := strings.Split(s, "/")
		if len(parts) < 3 || parts[0] != "" {
			continue
		}
		ip := net.ParseIP(parts[2])
		switch parts[1] {
		case "ip4", "ip6":
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if !ip.IsUnspecified() {
				add(s)
				continue
			}
			if local == nil {
				local = ips()
			}
			for _, lip := range local {
				if (parts[1] == "ip4") != (lip.To4() != nil) {
					continue
				}
				parts[2] = lip.String()
				add(strings.Join(parts, "/"))
			}
		default:
			add(s) // dns names and other transports are kept as given
		}
	}
	return out
}

// interfaceIPs lists the usable addresses of the up, non-loopback interfaces: all IPv4,
// and IPv6 global unicast (no link-local, no loopback).
func interfaceIPs() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var ips []net.IP
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() || ipn.IP.IsUnspecified() {
				continue
			}
			if ipn.IP.To4() == nil && !ipn.IP.IsGlobalUnicast() {
				continue
			}
			ips = append(ips, ipn.IP)
		}
	}
	return ips
}

// Link builds the share link for c (with optional path) naming this node as provider.
func (n *Local) Link(c cid.CID, p string) *link.Link {
	return &link.Link{CID: c, Path: p, Providers: []link.Provider{{Peer: n.PeerID(), Addrs: n.Addrs()}}}
}

// networkStatus is the Status section.
func (n *Local) networkStatus() *apiv0.NetworkStatus {
	ns := &apiv0.NetworkStatus{PeerID: n.PeerID().Base36(), Addrs: []string{}}
	for _, a := range n.Addrs() {
		ns.Addrs = append(ns.Addrs, a.String())
	}
	if n.Net != nil {
		ns.Serving = true
		for _, id := range n.Net.Connected() {
			ns.Connected = append(ns.Connected, id.Base36())
		}
		ns.Peers = len(ns.Connected)
	}
	return ns
}

// Share returns the link for a held CID target. The content must be complete in the
// store; the node is the provider hint. Serving=false says no host runs in this process
// (the caller starts one, or the content is served once a daemon runs).
func (n *Local) Share(ctx context.Context, req apiv0.ShareRequest) (*apiv0.ShareLink, error) {
	pt, err := target.Parse(req.Target)
	if err != nil {
		return nil, err
	}
	if pt.Kind != target.KindCID && pt.Kind != target.KindLink {
		return nil, apiv0.Errorf(apiv0.CodeInvalidArgument, "share needs a CID, not %s", pt.Kind).
			WithAction("give the CID printed by `" + nu.Name + " add`")
	}
	nd, root, p, err := n.resolve(pt.Raw)
	if err != nil {
		return nil, err
	}
	if err := n.requireComplete(ctx, nd.CID); err != nil {
		return nil, err
	}
	l := n.Link(root, p)
	res := &apiv0.ShareLink{Target: l.Target(), Link: l.String(), PeerID: l.Providers[0].Peer.Base36(), Addrs: []string{}, Serving: n.Net != nil}
	for _, a := range l.Providers[0].Addrs {
		res.Addrs = append(res.Addrs, a.String())
	}
	return res, nil
}

// requireComplete fails with not_found when anything reachable from c is missing.
func (n *Local) requireComplete(ctx context.Context, c cid.CID) error {
	st, err := n.Store.Walk(ctx, pinFor(c, ""), nil)
	if err != nil {
		return err
	}
	if st.Complete() {
		return nil
	}
	return missing(c, st.Missing, st.Objects)
}
