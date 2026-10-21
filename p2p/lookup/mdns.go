package lookup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/whyrusleeping/nu/p2p/wire"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

var ErrMDNSUnavailable = errors.New("lookup: no usable multicast interface")

type MDNSConfig struct {
	Peer      wire.PeerID
	Addresses func() []wire.Multiaddr
	ClusterID string
	// Nil selects up multicast interfaces. Explicit loopback interfaces are useful
	// for in-process tests; discovery can be completely disabled in lookup.Config.
	Interfaces []net.Interface
}

func ServiceName(cluster string) string {
	if cluster == "" {
		return "_p2p._udp.local."
	}
	hash := sha256.Sum256([]byte(cluster))
	return "_p2p-" + hex.EncodeToString(hash[:5]) + "._udp.local."
}

type multicastSocket struct {
	conn  *net.UDPConn
	group *net.UDPAddr
	iface net.Interface
}
type MDNS struct {
	config                      MDNSConfig
	cache                       *Cache
	service, instance, hostname string
	sockets                     []multicastSocket
	ctx                         context.Context
	cancel                      context.CancelFunc
	wg                          sync.WaitGroup
	once                        sync.Once
}

func NewMDNS(ctx context.Context, cfg MDNSConfig, cache *Cache) (*MDNS, error) {
	if cache == nil {
		return nil, wire.ErrFormat
	}
	if _, err := wire.PeerIDFromBytes(cfg.Peer.Bytes()); err != nil {
		return nil, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	m := &MDNS{config: cfg, cache: cache, service: ServiceName(cfg.ClusterID), ctx: child, cancel: cancel}
	m.hostname = hex.EncodeToString(random[:]) + ".local."
	m.instance = hex.EncodeToString(random[:]) + "." + m.service
	interfaces := cfg.Interfaces
	if interfaces == nil {
		all, err := net.Interfaces()
		if err != nil {
			cancel()
			return nil, err
		}
		for _, i := range all {
			if i.Flags&net.FlagUp != 0 && i.Flags&net.FlagMulticast != 0 && i.Flags&net.FlagLoopback == 0 && len(interfaceIPs(i)) > 0 {
				interfaces = append(interfaces, i)
			}
		}
	}
	if len(interfaces) > 32 {
		cancel()
		return nil, wire.ErrLimit
	}
	for _, iface := range interfaces {
		ips := interfaceIPs(iface)
		for _, family := range []int{4, 6} {
			available := false
			for _, ip := range ips {
				if (ip.To4() != nil) == (family == 4) {
					available = true
				}
			}
			if !available {
				continue
			}
			group := &net.UDPAddr{IP: net.ParseIP("224.0.0.251"), Port: 5353}
			network := "udp4"
			if family == 6 {
				group = &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: 5353, Zone: iface.Name}
				network = "udp6"
			}
			conn, err := net.ListenMulticastUDP(network, &iface, group)
			if err != nil {
				continue
			}
			if family == 4 {
				p := ipv4.NewPacketConn(conn)
				err = p.SetMulticastInterface(&iface)
				if err == nil {
					err = p.SetMulticastLoopback(true)
				}
				if err == nil {
					err = p.SetMulticastTTL(255)
				}
			} else {
				p := ipv6.NewPacketConn(conn)
				err = p.SetMulticastInterface(&iface)
				if err == nil {
					err = p.SetMulticastLoopback(true)
				}
				if err == nil {
					err = p.SetMulticastHopLimit(255)
				}
			}
			if err != nil {
				_ = conn.Close()
				continue
			}
			m.sockets = append(m.sockets, multicastSocket{conn, group, iface})
		}
	}
	if len(m.sockets) == 0 {
		cancel()
		return nil, ErrMDNSUnavailable
	}
	for _, socket := range m.sockets {
		m.wg.Add(1)
		go m.readLoop(socket)
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		<-child.Done()
		for _, s := range m.sockets {
			_ = s.conn.Close()
		}
	}()
	m.query()
	m.announce()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.query()
			case <-child.Done():
				return
			}
		}
	}()
	return m, nil
}
func interfaceIPs(iface net.Interface) []net.IP {
	addresses, _ := iface.Addrs()
	var ips []net.IP
	for _, a := range addresses {
		ip, _, err := net.ParseCIDR(a.String())
		if err == nil && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() {
			ips = append(ips, ip)
		}
	}
	return ips
}
func (m *MDNS) records(iface net.Interface) []dns.RR {
	if m.config.Addresses == nil {
		return nil
	}
	ips := interfaceIPs(iface)
	var texts []string
	var used []net.IP
	for _, address := range m.config.Addresses() {
		if len(texts) >= 8 {
			break
		}
		if address.Opaque() {
			continue
		}
		parsed, err := ma.NewMultiaddrBytes(address.Bytes())
		if err != nil {
			continue
		}
		parts := ma.Split(parsed)
		if len(parts) < 3 || (parts[0].Protocol().Code != ma.P_IP4 && parts[0].Protocol().Code != ma.P_IP6) || parts[1].Protocol().Code != ma.P_UDP || parts[2].Protocol().Code != ma.P_QUIC_V1 {
			continue
		}
		ip := net.ParseIP(parts[0].Value())
		if ip == nil {
			continue
		}
		for _, local := range ips {
			if (local.To4() != nil) != (ip.To4() != nil) || (!ip.IsUnspecified() && !ip.Equal(local)) {
				continue
			}
			text := address.String()
			if ip.IsUnspecified() {
				text = strings.Replace(text, "/"+ip.String()+"/", "/"+local.String()+"/", 1)
			}
			a, err := wire.ParseMultiaddr(text, 1024)
			if err != nil {
				continue
			}
			a, err = NormalizeAddress(m.config.Peer, a)
			if err != nil {
				continue
			}
			text = "dnsaddr=" + a.String()
			if len(text) > 255 || len(texts) >= 8 {
				continue
			}
			texts = append(texts, text)
			used = append(used, local)
		}
	}
	if len(texts) == 0 {
		return nil
	}
	hdr := func(name string, kind uint16, flush bool) dns.RR_Header {
		class := uint16(dns.ClassINET)
		if flush {
			class |= 0x8000
		}
		return dns.RR_Header{Name: name, Rrtype: kind, Class: class, Ttl: 120}
	}
	records := []dns.RR{&dns.PTR{Hdr: hdr(m.service, dns.TypePTR, false), Ptr: m.instance}, &dns.SRV{Hdr: hdr(m.instance, dns.TypeSRV, true), Port: 4001, Target: m.hostname}, &dns.TXT{Hdr: hdr(m.instance, dns.TypeTXT, true), Txt: texts}}
	for _, ip := range used {
		if ip.To4() != nil {
			records = append(records, &dns.A{Hdr: hdr(m.hostname, dns.TypeA, true), A: ip.To4()})
		} else {
			records = append(records, &dns.AAAA{Hdr: hdr(m.hostname, dns.TypeAAAA, true), AAAA: ip})
		}
	}
	return records
}
func (m *MDNS) send(socket multicastSocket, msg *dns.Msg, to *net.UDPAddr) {
	msg.Compress = true
	b, err := msg.Pack()
	if err != nil || len(b) > 9000 {
		return
	}
	_, _ = socket.conn.WriteToUDP(b, to)
}
func (m *MDNS) query() {
	q := &dns.Msg{Question: []dns.Question{{Name: m.service, Qtype: dns.TypePTR, Qclass: dns.ClassINET}}}
	for _, s := range m.sockets {
		m.send(s, q, s.group)
	}
}
func (m *MDNS) announce() {
	for _, s := range m.sockets {
		records := m.records(s.iface)
		if len(records) > 0 {
			m.send(s, &dns.Msg{MsgHdr: dns.MsgHdr{Response: true, Authoritative: true}, Answer: records[:1], Extra: records[1:]}, s.group)
		}
	}
}

// DecodeMDNS bounds DNS packet size and RR counts before interpreting TXT
// claims. Only service-scoped dnsaddr records with destination PeerIds count.
func DecodeMDNS(packet []byte, service string, maxBytes, maxRecords int) ([]Advertisement, error) {
	if maxBytes < 0 || len(packet) > maxBytes {
		return nil, wire.ErrLimit
	}
	if err := checkDNSCounts(packet, maxRecords); err != nil {
		return nil, err
	}
	var msg dns.Msg
	if err := msg.Unpack(packet); err != nil {
		return nil, err
	}
	if len(msg.Answer)+len(msg.Extra)+len(msg.Ns) > maxRecords {
		return nil, wire.ErrLimit
	}
	if !msg.Response {
		return nil, nil
	}
	var out []Advertisement
	for _, rr := range append(msg.Answer, msg.Extra...) {
		txt, ok := rr.(*dns.TXT)
		if !ok || !strings.HasSuffix(strings.ToLower(txt.Hdr.Name), "."+strings.ToLower(service)) {
			continue
		}
		for _, text := range txt.Txt {
			if !strings.HasPrefix(text, "dnsaddr=") {
				continue
			}
			if len(out) >= 32 {
				return nil, wire.ErrLimit
			}
			addr, err := wire.ParseMultiaddr(strings.TrimPrefix(text, "dnsaddr="), 1024)
			if err != nil {
				continue
			}
			id, err := AddressPeer(addr)
			if err != nil {
				continue
			}
			out = append(out, Advertisement{Hint: Hint{Peer: id, Addresses: []wire.Multiaddr{addr}}, TTL: time.Duration(min(txt.Hdr.Ttl, 120)) * time.Second})
		}
	}
	return out, nil
}

func checkDNSCounts(b []byte, maxRecords int) error {
	if len(b) < 12 {
		return wire.ErrFormat
	}
	count := 0
	for _, offset := range []int{4, 6, 8, 10} {
		count += int(binary.BigEndian.Uint16(b[offset : offset+2]))
	}
	if maxRecords < 0 || count > maxRecords {
		return wire.ErrLimit
	}
	return nil
}

type Advertisement struct {
	Hint Hint
	TTL  time.Duration
}

func (m *MDNS) readLoop(socket multicastSocket) {
	defer m.wg.Done()
	packet := make([]byte, 9001)
	for {
		n, from, err := socket.conn.ReadFromUDP(packet)
		if err != nil {
			return
		}
		if n > 9000 {
			continue
		}
		if err := checkDNSCounts(packet[:n], 128); err != nil {
			continue
		}
		var msg dns.Msg
		if err := msg.Unpack(packet[:n]); err != nil {
			continue
		}
		if !msg.Response {
			if len(msg.Question) > 32 {
				continue
			}
			respond := false
			unicast := from.Port != 5353
			for _, q := range msg.Question {
				if strings.EqualFold(q.Name, m.service) || strings.EqualFold(q.Name, m.instance) || strings.EqualFold(q.Name, m.hostname) {
					respond = true
					unicast = unicast || q.Qclass&0x8000 != 0
				}
			}
			if respond {
				records := m.records(socket.iface)
				if len(records) > 0 {
					to := socket.group
					if unicast {
						to = from
					}
					m.send(socket, &dns.Msg{MsgHdr: dns.MsgHdr{Id: msg.Id, Response: true, Authoritative: true}, Answer: records[:1], Extra: records[1:]}, to)
				}
			}
			continue
		}
		ads, err := DecodeMDNS(packet[:n], m.service, 9000, 128)
		if err != nil {
			continue
		}
		for _, ad := range ads {
			if ad.Hint.Peer != m.config.Peer {
				_ = m.cache.Remember(m.ctx, ad.Hint, "mdns", 0.5, ad.TTL)
			}
		}
	}
}
func (m *MDNS) Lookup(ctx context.Context, id wire.PeerID, emit func(Address) bool) error {
	m.query()
	sent := make(map[string]bool)
	for {
		m.cache.mu.Lock()
		addresses := m.cache.addressesLocked(id)
		changed := m.cache.changed
		m.cache.mu.Unlock()
		for _, a := range addresses {
			if a.Source != "mdns" || sent[a.Addr.String()] {
				continue
			}
			sent[a.Addr.String()] = true
			if !emit(a) {
				return ctx.Err()
			}
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-m.ctx.Done():
			return m.ctx.Err()
		}
	}
}
func (m *MDNS) Close() error   { m.once.Do(func() { m.cancel(); m.wg.Wait() }); return nil }
func (m *MDNS) String() string { return fmt.Sprintf("mDNS %s", m.service) }
