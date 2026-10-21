package kad

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	ma "github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/nu/p2p/lookup"
	"github.com/whyrusleeping/nu/p2p/wire"
)

var defaultBootstrapIDs = []string{
	"QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
	"QmQCU2EcMqAqQPR2i9bChDtGNJchTbq5TbXJJ16u19uLTa",
	"QmbLHAnMoJPWSCR5Zhtx6BHJX9KiKNN6tpvbUcqanj75Nb",
	"QmcZf59bWwK5XFi76CZX8cbJ4BhTzzA3gU1ZjYZcYW3dwt",
	"QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",
}

// BootstrapDNS resolves deployed default bootstrap peers with the bounded
// dnsaddr resolver, then resolves DNS4/DNS6 address components to literals.
// Only UDP/QUIC-v1 addresses survive because the Nu host dials literal IPs.
func (s *Service) BootstrapDNS(ctx context.Context, dns *lookup.DNS) error {
	if dns == nil {
		return wire.ErrFormat
	}
	var errs []error
	for _, text := range defaultBootstrapIDs {
		id, err := wire.ParsePeerID(text)
		if err != nil {
			return err
		}
		root, err := wire.ParseMultiaddr("/dnsaddr/bootstrap.libp2p.io/p2p/"+id.String(), 1024)
		if err != nil {
			return err
		}
		var addrs [][]byte
		err = dns.Resolve(ctx, id, root, func(a lookup.Address) bool {
			if a.Peer == id && isQUICAddress(a.Addr) {
				resolved, e := resolveQUICAddress(ctx, id, a.Addr)
				if e != nil {
					errs = append(errs, e)
					return true
				}
				for _, addr := range resolved {
					if !s.allowRoutingAddress(addr) {
						continue
					}
					addrs = append(addrs, addr.Bytes())
					if len(addrs) >= MaxAddrs {
						return false
					}
				}
			}
			return true
		})
		if err != nil {
			errs = append(errs, err)
		}
		if len(addrs) > 0 {
			s.table.Add(PeerInfo{ID: id, Addrs: addrs})
		}
	}
	return errors.Join(errs...)
}

func resolveQUICAddress(ctx context.Context, id wire.PeerID, a wire.Multiaddr) ([]wire.Multiaddr, error) {
	m, err := ma.NewMultiaddrBytes(a.Bytes())
	if err != nil {
		return nil, err
	}
	parts := ma.Split(m)
	if len(parts) != 4 || parts[1].Protocol().Code != ma.P_UDP || parts[2].Protocol().Code != ma.P_QUIC_V1 || parts[3].Protocol().Code != ma.P_P2P {
		return nil, wire.ErrFormat
	}
	peer, err := wire.ParsePeerID(parts[3].Value())
	if err != nil || peer != id {
		return nil, wire.ErrFormat
	}
	port, err := strconv.Atoi(parts[1].Value())
	if err != nil || port < 1 || port > 65535 {
		return nil, wire.ErrFormat
	}
	protocol := parts[0].Protocol().Code
	var ips []net.IP
	switch protocol {
	case ma.P_IP4:
		ip := net.ParseIP(parts[0].Value())
		if ip == nil || ip.To4() == nil {
			return nil, wire.ErrFormat
		}
		ips = []net.IP{ip.To4()}
	case ma.P_IP6:
		ip := net.ParseIP(parts[0].Value())
		if ip == nil || ip.To4() != nil {
			return nil, wire.ErrFormat
		}
		ips = []net.IP{ip}
	case ma.P_DNS, ma.P_DNS4, ma.P_DNS6:
		host := parts[0].Value()
		lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		ips, err = net.DefaultResolver.LookupIP(lookupCtx, "ip", host)
		cancel()
		if err != nil {
			return nil, err
		}
	default:
		return nil, wire.ErrFormat
	}
	var out []wire.Multiaddr
	for _, ip := range ips {
		family, addr := "ip6", ip.String()
		if v4 := ip.To4(); v4 != nil {
			family, addr = "ip4", v4.String()
		}
		if protocol == ma.P_DNS4 && family != "ip4" || protocol == ma.P_DNS6 && family != "ip6" {
			continue
		}
		text := strings.Join([]string{"", family, addr, "udp", strconv.Itoa(port), "quic-v1", "p2p", id.String()}, "/")
		literal, e := wire.ParseMultiaddr(text, 1024)
		if e == nil {
			out = append(out, literal)
		}
		if len(out) >= MaxAddrs {
			break
		}
	}
	if len(out) == 0 {
		return nil, wire.ErrFormat
	}
	return out, nil
}

func (s *Service) allowRoutingAddress(a wire.Multiaddr) bool {
	if s.cfg.AllowPrivateAddresses {
		return true
	}
	m, err := ma.NewMultiaddrBytes(a.Bytes())
	if err != nil {
		return false
	}
	parts := ma.Split(m)
	if len(parts) != 4 {
		return false
	}
	code := parts[0].Protocol().Code
	if code != ma.P_IP4 && code != ma.P_IP6 {
		return false
	}
	ip := net.ParseIP(parts[0].Value())
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified()
}

func IsQUICAddress(a wire.Multiaddr, id wire.PeerID) bool {
	text := a.String()
	return strings.HasSuffix(text, "/quic-v1/p2p/"+id.String()) && strings.Contains(text, "/udp/")
}
