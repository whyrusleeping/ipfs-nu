package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/netcheck"
	"github.com/whyrusleeping/nu/p2p/proto/circuit"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/relay"
)

// DefaultRelayListen is the relay's default listener (networking D8/D10: QUIC on UDP 443).
const DefaultRelayListen = "/ip4/0.0.0.0/udp/443/quic-v1"

// listFlag collects a repeatable flag; each occurrence may also hold comma-separated values.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error {
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			*l = append(*l, v)
		}
	}
	return nil
}

// relayStatus is what `nu relay` prints once it serves.
type relayStatus struct {
	PeerID    string   `json:"peer_id"`
	Listen    []string `json:"listen"`    // bound addresses as /ip/udp/quic-v1/p2p
	Advertise []string `json:"advertise"` // addresses handed to reserving peers
	Admitted  int      `json:"admitted"`
	Netcheck  []string `json:"netcheck,omitempty"` // NAT-profiling observer UDP sockets (0018)
}

var cmdRelay = &command{
	name:    "relay",
	args:    "[--listen <addr>]... [--advertise <addr>]... [--admit <peer-id>]... [--netcheck <ip:port>]...",
	summary: "run a circuit relay (identify, AutoNAT v2, circuit v2 HOP) with this repo's identity",
	run: func(ctx context.Context, e *env, args []string) error {
		fs := e.flags()
		var listen, advertise, admit listFlag
		fs.Var(&listen, "listen", "listen multiaddr or host:port, repeatable (default relay.listen or "+DefaultRelayListen+")")
		fs.Var(&advertise, "advertise", "public address to hand to reserving peers, repeatable; required with a wildcard listener")
		fs.Var(&admit, "admit", "peer id admitted to full-service circuits, repeatable")
		maxRes := fs.Int("max-reservations", 0, "reservation slots (default 128)")
		resPerIP := fs.Int("reservations-per-ip", 0, "reservations per observed IP (default 8)")
		maxCirc := fs.Int("max-circuits", 0, "concurrent circuits (default 128)")
		circPerPeer := fs.Int("circuits-per-peer", 0, "circuits per peer (default 8)")
		circPerIP := fs.Int("circuits-per-ip", 0, "circuits per observed IP (default 16)")
		ttl := fs.Duration("reservation-ttl", 0, "reservation lifetime (default 1h)")
		var ncPrimary, ncAltIP listFlag
		fs.Var(&ncPrimary, "netcheck", "serve /nu/netcheck/1 NAT profiling from this literal UDP ip:port, one per address family (repeatable)")
		ncAltPort := fs.Int("netcheck-alt-port", 0, "alternate UDP port on each --netcheck IP (enables port-filtering and mapping classes)")
		fs.Var(&ncAltIP, "netcheck-alt-ip", "second local IP of the same family as a --netcheck address, same port (enables address-filtering tests)")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return usagef("relay takes no arguments")
		}
		r, err := e.openRepo()
		if err != nil {
			return err
		}
		cfg, err := r.Config()
		if err != nil {
			return err
		}
		rc := repo.RelayConfig{}
		if cfg.Relay != nil {
			rc = *cfg.Relay
		}
		// Flags override config; config overrides defaults.
		if len(listen) == 0 {
			listen = rc.Listen
		}
		if len(listen) == 0 {
			listen = listFlag{DefaultRelayListen}
		}
		if len(advertise) == 0 {
			advertise = rc.Advertise
		}
		if len(admit) == 0 {
			admit = rc.Admit
		}
		pick := func(flag, conf int) int {
			if flag != 0 {
				return flag
			}
			return conf
		}
		srv := circuit.ServerConfig{
			MaxReservations: pick(*maxRes, rc.MaxReservations), ReservationsPerIP: pick(*resPerIP, rc.ReservationsPerIP),
			MaxCircuits: pick(*maxCirc, rc.MaxCircuits), CircuitsPerPeer: pick(*circPerPeer, rc.CircuitsPerPeer),
			CircuitsPerIP: pick(*circPerIP, rc.CircuitsPerIP), ReservationTTL: *ttl,
		}
		if srv.ReservationTTL == 0 && rc.ReservationTTL != "" {
			if srv.ReservationTTL, err = time.ParseDuration(rc.ReservationTTL); err != nil {
				return apiv0.Errorf(apiv0.CodeInvalidArgument, "relay.reservation_ttl: %v", err)
			}
		}

		hostListen, wildcard, err := relayListenAddrs(listen)
		if err != nil {
			return err
		}
		if wildcard && len(advertise) == 0 {
			return usagef("a wildcard listener needs the public address peers should use: --advertise /ip4/<public-ip>/udp/<port>/quic-v1 (or relay.advertise)")
		}
		// The relay and the daemon share node.key; the lock keeps one PeerId on one host.
		lock, err := r.Lock()
		if err != nil {
			return apiv0.Errorf(apiv0.CodeUnavailable, "%v", err).
				WithAction("stop it first; the relay uses the same identity (" + r.Path(repo.KeyFile) + ")")
		}
		defer lock.Release()
		key, err := node.LoadKey(r)
		if err != nil {
			return err
		}
		id := key.Public().PeerID()
		for _, a := range advertise {
			ma, err := relayAdvertiseAddr(a, id)
			if err != nil {
				return apiv0.Errorf(apiv0.CodeInvalidArgument, "--advertise %s: %v", a, err).
					WithAction("use /ip4|ip6/<public-ip>/udp/<port>/quic-v1")
			}
			srv.Addresses = append(srv.Addresses, ma)
		}
		for _, p := range admit {
			pid, err := wire.ParsePeerID(p)
			if err != nil {
				return apiv0.Errorf(apiv0.CodeInvalidArgument, "--admit %s: %v", p, err)
			}
			srv.Admitted = append(srv.Admitted, pid)
		}

		nc, err := relayNetcheckConfig(ncPrimary, *ncAltPort, ncAltIP)
		if err != nil {
			return err
		}
		s, err := relay.New(relay.Config{Host: host.Config{Key: key, Listen: hostListen}, Server: srv, Netcheck: nc})
		if err != nil {
			return apiv0.Errorf(apiv0.CodeFailed, "cannot start the relay: %v", err).
				WithAction("check that the listen port is free and bindable (UDP 443 may need privileges), and that limits are within range")
		}
		defer s.Close()
		// 0016 hook: the dnsaddr/peer-routing server attaches to s.Host here, once it exists.

		st := relayStatus{PeerID: id.Base36(), Admitted: len(srv.Admitted)}
		for _, a := range s.Host.Addrs() {
			st.Listen = append(st.Listen, a.String()+"/p2p/"+id.String())
		}
		for _, a := range srv.Addresses {
			st.Advertise = append(st.Advertise, a.String())
		}
		if len(st.Advertise) == 0 {
			st.Advertise = st.Listen
		}
		if s.Netcheck != nil {
			for _, set := range s.Netcheck.Descriptor().Sets {
				for _, ep := range []netip.AddrPort{set.Primary, set.AltPort, set.AltIP} {
					if ep.IsValid() {
						st.Netcheck = append(st.Netcheck, ep.String())
					}
				}
			}
		}
		if err := e.emit(st, func(w io.Writer) {
			fmt.Fprintf(w, "%s %s relay serving (identify, AutoNAT v2, circuit v2 HOP)\n  peer id    %s\n  listen     %s\n  advertise  %s\n  admitted   %d peers\n",
				nu.Name, nu.Version, st.PeerID, strings.Join(st.Listen, ", "), strings.Join(st.Advertise, ", "), st.Admitted)
			if len(st.Netcheck) > 0 {
				fmt.Fprintf(w, "  netcheck   %s\n", strings.Join(st.Netcheck, ", "))
			}
			fmt.Fprintln(w, "press Ctrl-C to stop")
		}); err != nil {
			return err
		}
		<-ctx.Done()
		return nil
	},
}

// relayListenAddrs converts listen entries (`/ip4/<ip>/udp/<port>/quic-v1` or host:port)
// to host.Config.Listen strings and reports whether any binds an unspecified IP.
func relayListenAddrs(listen []string) ([]string, bool, error) {
	var out []string
	wildcard := false
	for _, s := range listen {
		hostPort := s
		if strings.HasPrefix(s, "/") {
			parts := strings.Split(strings.TrimPrefix(s, "/"), "/")
			if len(parts) != 5 || (parts[0] != "ip4" && parts[0] != "ip6") || parts[2] != "udp" || parts[4] != "quic-v1" {
				return nil, false, apiv0.Errorf(apiv0.CodeInvalidArgument, "listen address %q is not /ip4|ip6/<ip>/udp/<port>/quic-v1", s)
			}
			hostPort = net.JoinHostPort(parts[1], parts[3])
		}
		h, p, err := net.SplitHostPort(hostPort)
		if err != nil {
			return nil, false, apiv0.Errorf(apiv0.CodeInvalidArgument, "listen address %q: %v", s, err)
		}
		ip, err := netip.ParseAddr(h)
		if err != nil {
			return nil, false, apiv0.Errorf(apiv0.CodeInvalidArgument, "listen address %q: %v", s, err)
		}
		if _, err := strconv.ParseUint(p, 10, 16); err != nil {
			return nil, false, apiv0.Errorf(apiv0.CodeInvalidArgument, "listen address %q: bad port", s)
		}
		wildcard = wildcard || ip.IsUnspecified()
		out = append(out, net.JoinHostPort(ip.String(), p))
	}
	return out, wildcard, nil
}

// relayNetcheckConfig builds the 0018 observer sets from the --netcheck flags
// (nil when none are given). Observer sockets need literal, non-wildcard IPs.
func relayNetcheckConfig(primaries []string, altPort int, altIPs []string) (*netcheck.ServerConfig, error) {
	if len(primaries) == 0 {
		if altPort != 0 || len(altIPs) != 0 {
			return nil, usagef("--netcheck-alt-port/--netcheck-alt-ip need --netcheck <ip:port>")
		}
		return nil, nil
	}
	if altPort < 0 || altPort > 65535 {
		return nil, usagef("--netcheck-alt-port must be a UDP port")
	}
	cfg := &netcheck.ServerConfig{}
	used := make([]bool, len(altIPs))
	for _, p := range primaries {
		ep, err := netip.ParseAddrPort(p)
		if err != nil || ep.Addr().IsUnspecified() || ep.Port() == 0 {
			return nil, usagef("--netcheck %q must be a literal ip:port (no wildcard or zero port)", p)
		}
		set := netcheck.EndpointSet{Primary: netip.AddrPortFrom(ep.Addr().Unmap(), ep.Port())}
		if altPort != 0 {
			set.AltPort = netip.AddrPortFrom(set.Primary.Addr(), uint16(altPort))
		}
		for i, a := range altIPs {
			ip, err := netip.ParseAddr(a)
			if err != nil {
				return nil, usagef("--netcheck-alt-ip %q is not an IP", a)
			}
			if !used[i] && ip.Unmap().Is4() == set.Primary.Addr().Is4() {
				used[i], set.AltIP = true, netip.AddrPortFrom(ip.Unmap(), set.Primary.Port())
				break
			}
		}
		cfg.Sets = append(cfg.Sets, netcheck.ServerSet{Listen: set})
	}
	for i, u := range used {
		if !u {
			return nil, usagef("--netcheck-alt-ip %s has no --netcheck address of its family", altIPs[i])
		}
	}
	return cfg, nil
}

// relayAdvertiseAddr parses a public relay address, appending /p2p/<id> when absent.
// circuit.NewServer rejects wildcard, multicast and zero-port addresses.
func relayAdvertiseAddr(s string, id wire.PeerID) (wire.Multiaddr, error) {
	if !strings.Contains(s, "/p2p/") {
		s = strings.TrimSuffix(s, "/") + "/p2p/" + id.String()
	}
	return wire.ParseMultiaddr(s, 1024)
}
