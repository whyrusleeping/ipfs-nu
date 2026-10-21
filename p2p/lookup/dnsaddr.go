package lookup

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// DNSLimits bound one complete recursive lookup, not just each answer.
type DNSLimits struct{ MaxBytes, MaxRecords, MaxDepth, MaxQueries, MaxAddresses, MaxAddressBytes int }

func DefaultDNSLimits() DNSLimits { return DNSLimits{8192, 128, 8, 16, 32, 1024} }

type DNSConfig struct {
	// Domain is the peer namespace, e.g. peers.example.org. Roots optionally add
	// bootstrap/explicit /dnsaddr hints. Neither source is enabled implicitly.
	Domain  string
	Roots   []wire.Multiaddr
	Servers []string // literal IP:port; nil loads /etc/resolv.conf
	Limits  DNSLimits
	Timeout time.Duration
}
type DNS struct{ cfg DNSConfig }

func NewDNS(cfg DNSConfig) (*DNS, error) {
	if cfg.Limits == (DNSLimits{}) {
		cfg.Limits = DefaultDNSLimits()
	}
	l := cfg.Limits
	if l.MaxBytes < 12 || l.MaxBytes > 65535 || l.MaxRecords <= 0 || l.MaxDepth <= 0 || l.MaxQueries <= 0 || l.MaxAddresses <= 0 || l.MaxAddressBytes <= 0 {
		return nil, wire.ErrLimit
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 3 * time.Second
	}
	if cfg.Timeout <= 0 || len(cfg.Roots) > 8 || len(cfg.Servers) > 8 {
		return nil, wire.ErrLimit
	}
	if cfg.Domain != "" {
		var err error
		cfg.Domain, err = peerDomain(cfg.Domain)
		if err != nil {
			return nil, err
		}
	}
	if cfg.Servers == nil {
		resolv, err := dns.ClientConfigFromFile("/etc/resolv.conf")
		if err != nil {
			return nil, err
		}
		for _, ip := range resolv.Servers {
			cfg.Servers = append(cfg.Servers, net.JoinHostPort(ip, resolv.Port))
			if len(cfg.Servers) == 3 {
				break
			}
		}
	}
	if len(cfg.Servers) == 0 {
		return nil, wire.ErrFormat
	}
	for _, s := range cfg.Servers {
		if _, err := netip.ParseAddrPort(s); err != nil {
			return nil, err
		}
	}
	cfg.Servers = append([]string(nil), cfg.Servers...)
	cfg.Roots = append([]wire.Multiaddr(nil), cfg.Roots...)
	return &DNS{cfg}, nil
}
func peerDomain(s string) (string, error) {
	s = strings.TrimSuffix(strings.ToLower(s), ".")
	if len(s) == 0 || len(s) > 180 {
		return "", wire.ErrLimit
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", wire.ErrFormat
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return "", wire.ErrFormat
			}
		}
	}
	return s, nil
}

// DNSName uses a lowercase CIDv1 libp2p-key base36 label, not a base58 DNS label.
func DNSName(id wire.PeerID, domain string) (string, error) {
	if _, err := wire.PeerIDFromBytes(id.Bytes()); err != nil {
		return "", err
	}
	d, err := peerDomain(domain)
	if err != nil {
		return "", err
	}
	return "_dnsaddr." + id.Base36() + "." + d + ".", nil
}
func (d *DNS) Lookup(ctx context.Context, id wire.PeerID, emit func(Address) bool) error {
	roots := append([]wire.Multiaddr(nil), d.cfg.Roots...)
	if d.cfg.Domain != "" {
		name, err := DNSName(id, d.cfg.Domain)
		if err != nil {
			return err
		}
		a, err := wire.ParseMultiaddr("/dnsaddr/"+strings.TrimSuffix(strings.TrimPrefix(name, "_dnsaddr."), ".")+"/p2p/"+id.String(), 1024)
		if err != nil {
			return err
		}
		roots = append(roots, a)
	}
	return d.resolve(ctx, id, roots, emit)
}

// Resolve resolves one explicit dnsaddr hint, enforcing suffix matching at each
// recursion hop and binding all emitted addresses to the requested destination.
func (d *DNS) Resolve(ctx context.Context, id wire.PeerID, root wire.Multiaddr, emit func(Address) bool) error {
	return d.resolve(ctx, id, []wire.Multiaddr{root}, emit)
}
func (d *DNS) resolve(ctx context.Context, id wire.PeerID, roots []wire.Multiaddr, emit func(Address) bool) error {
	if _, err := wire.PeerIDFromBytes(id.Bytes()); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()
	l := d.cfg.Limits
	queries, records, count := 0, 0, 0
	visited := map[string]bool{}
	seen := map[string]bool{}
	var walk func(wire.Multiaddr, int, time.Time) error
	var stopped bool
	walk = func(a wire.Multiaddr, depth int, expiry time.Time) error {
		if stopped {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(a.Bytes()) > l.MaxAddressBytes {
			return wire.ErrLimit
		}
		m, err := ma.NewMultiaddrBytes(a.Bytes())
		if err != nil || len(m) == 0 {
			return wire.ErrFormat
		}
		parts := ma.Split(m)
		if parts[0].Protocol().Code != ma.P_DNSADDR {
			normalized, err := NormalizeAddress(id, a)
			if err != nil {
				return nil
			} // other peers are not matches
			key := string(normalized.Bytes())
			if seen[key] {
				return nil
			}
			if count >= l.MaxAddresses {
				return wire.ErrLimit
			}
			seen[key] = true
			count++
			now := time.Now()
			if expiry.Before(now) {
				expiry = now
			} // zero TTL: usable now, never cacheable
			stopped = !emit(Address{Peer: id, Addr: normalized, Source: "dnsaddr", Seen: now, Expires: expiry, Confidence: 0.6})
			return nil
		}
		if depth >= l.MaxDepth {
			return wire.ErrLimit
		}
		key := string(a.Bytes())
		if visited[key] {
			return nil
		}
		visited[key] = true
		domain, err := peerDomain(parts[0].Value())
		if err != nil {
			return err
		}
		name := "_dnsaddr." + domain + "."
		suffix := parts[1:]
		if queries >= l.MaxQueries {
			return wire.ErrLimit
		}
		queries++
		msg, err := d.query(ctx, name)
		if err != nil {
			return err
		}
		records += len(msg.Answer) + len(msg.Ns) + len(msg.Extra)
		if records > l.MaxRecords {
			return wire.ErrLimit
		}
		// Recursive DNS servers may include a CNAME chain. Only consume TXT
		// belonging to the queried name or one of those aliases, retaining TTLs.
		names := map[string]time.Time{name: expiry}
		for hop := 0; hop < l.MaxDepth; hop++ {
			changed := false
			for _, rr := range msg.Answer {
				alias, ok := rr.(*dns.CNAME)
				if !ok {
					continue
				}
				parent, ok := names[strings.ToLower(alias.Hdr.Name)]
				target := strings.ToLower(alias.Target)
				if _, exists := names[target]; !ok || exists {
					continue
				}
				until := time.Now().Add(time.Duration(alias.Hdr.Ttl) * time.Second)
				if parent.Before(until) {
					until = parent
				}
				names[target] = until
				changed = true
			}
			if !changed {
				break
			}
		}
		for _, rr := range msg.Answer {
			txt, ok := rr.(*dns.TXT)
			if !ok {
				continue
			}
			ownerExpiry, ok := names[strings.ToLower(txt.Hdr.Name)]
			if !ok {
				continue
			}
			child, err := ParseDNSADDRTXT(txt.Txt, l.MaxAddressBytes)
			if err != nil {
				continue
			}
			childAddr, err := ma.NewMultiaddrBytes(child.Bytes())
			if err != nil {
				continue
			}
			childParts := ma.Split(childAddr)
			if len(childParts) < len(suffix) {
				continue
			}
			matches := true
			for i, part := range suffix {
				if !bytes.Equal(part.Bytes(), childParts[len(childParts)-len(suffix)+i].Bytes()) {
					matches = false
					break
				}
			}
			if !matches {
				continue
			}
			ttl := min(time.Duration(txt.Hdr.Ttl)*time.Second, 5*time.Minute)
			nextExpiry := time.Now().Add(ttl)
			if ownerExpiry.Before(nextExpiry) {
				nextExpiry = ownerExpiry
			}
			if err = walk(child, depth+1, nextExpiry); err != nil {
				return err
			}
		}
		return nil
	}
	for _, root := range roots {
		root, err := NormalizeAddress(id, root)
		if err != nil {
			return err
		}
		if err := walk(root, 0, time.Now().Add(5*time.Minute)); err != nil {
			return err
		}
	}
	return nil
}

// ParseDNSADDRTXT joins the character-strings of ONE TXT RR (not different RRs).
func ParseDNSADDRTXT(parts []string, maxBytes int) (wire.Multiaddr, error) {
	total := 0
	for _, s := range parts {
		total += len(s)
		if total > maxBytes+8 || maxBytes <= 0 {
			return wire.Multiaddr{}, wire.ErrLimit
		}
	}
	text := strings.Join(parts, "")
	if !strings.HasPrefix(text, "dnsaddr=") {
		return wire.Multiaddr{}, wire.ErrFormat
	}
	return wire.ParseMultiaddr(strings.TrimPrefix(text, "dnsaddr="), maxBytes)
}
func parseDNSMessage(b []byte, l DNSLimits) (*dns.Msg, error) {
	if len(b) < 12 || len(b) > l.MaxBytes {
		return nil, wire.ErrLimit
	}
	records := 0
	for _, off := range []int{4, 6, 8, 10} {
		records += int(binary.BigEndian.Uint16(b[off : off+2]))
	}
	if records > l.MaxRecords {
		return nil, wire.ErrLimit
	}
	var msg dns.Msg
	if err := msg.Unpack(b); err != nil {
		return nil, err
	}
	return &msg, nil
}
func (d *DNS) query(ctx context.Context, name string) (*dns.Msg, error) {
	req := new(dns.Msg)
	req.SetQuestion(name, dns.TypeTXT)
	req.SetEdns0(1232, false)
	packet, err := req.Pack()
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, server := range d.cfg.Servers {
		for _, network := range []string{"udp", "tcp"} {
			b, err := exchangeDNS(ctx, network, server, packet, d.cfg.Limits.MaxBytes)
			if err != nil {
				errs = append(errs, err)
				break
			}
			reply, err := parseDNSMessage(b, d.cfg.Limits)
			if err != nil {
				return nil, err
			}
			if reply.Id != req.Id || !reply.Response || reply.Opcode != dns.OpcodeQuery || len(reply.Question) != 1 || reply.Question[0] != req.Question[0] {
				return nil, wire.ErrFormat
			}
			if reply.Truncated && network == "udp" {
				continue
			}
			if reply.Truncated {
				return nil, wire.ErrLimit
			}
			if reply.Rcode != dns.RcodeSuccess && reply.Rcode != dns.RcodeNameError {
				return nil, errors.New("lookup: DNS server failure")
			}
			return reply, nil
		}
	}
	return nil, errors.Join(errs...)
}
func exchangeDNS(ctx context.Context, network, server string, packet []byte, maxBytes int) ([]byte, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if network == "tcp" {
		framed := binary.BigEndian.AppendUint16(nil, uint16(len(packet)))
		framed = append(framed, packet...)
		if _, err = conn.Write(framed); err != nil {
			return nil, err
		}
		var prefix [2]byte
		if _, err = io.ReadFull(conn, prefix[:]); err != nil {
			return nil, err
		}
		n := int(binary.BigEndian.Uint16(prefix[:]))
		if n > maxBytes {
			return nil, wire.ErrLimit
		}
		b := make([]byte, n)
		_, err = io.ReadFull(conn, b)
		return b, err
	}
	if _, err = conn.Write(packet); err != nil {
		return nil, err
	}
	b := make([]byte, maxBytes+1)
	n, err := conn.Read(b)
	if n > maxBytes {
		return nil, wire.ErrLimit
	}
	return b[:n], err
}
