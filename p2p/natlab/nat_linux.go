//go:build linux && natlab

package natlab

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

type Mapping string

const (
	// Kernel uses ordinary Linux conntrack NAT; it is NOT a promised RFC class.
	Kernel               Mapping = "kernel"
	EndpointIndependent  Mapping = "endpoint-independent"
	AddressDependent     Mapping = "address-dependent"
	AddressPortDependent Mapping = "address-port-dependent"
)

type Filtering string

const (
	FilterEndpointIndependent  Filtering = "endpoint-independent"
	FilterAddressDependent     Filtering = "address-dependent"
	FilterAddressPortDependent Filtering = "address-port-dependent"
)

type Allocation string

const (
	Preserve   Allocation = "preserve"
	Sequential Allocation = "sequential"
	Random     Allocation = "random"
)

// NATConfig has two modes. Kernel uses native masquerade (all source ports).
// Other mappings are bounded fixtures: enumerate inside UDP endpoints and every
// external destination. Each mapping class reserves a distinct public port and
// installs explicit SNAT/DNAT, avoiding Linux's opportunistic port reuse being
// mistaken for EIM. Unlisted inside endpoints and destination classes are dropped
// (EIM permits any destination; ADM permits all ports on listed IPs).
// Fixture allocations are made at installation, not on each new flow.
type NATConfig struct {
	WAN, LAN          string
	ExternalIP        netip.Addr
	Inside            []netip.AddrPort
	Destinations      []netip.AddrPort
	Mapping           Mapping
	Filtering         Filtering
	Allocation        Allocation
	Hairpin, BlockUDP bool
	// Lifetime bounds idle fixture pinholes/learned filters, not conntrack flows.
	// Existing conntrack flows can outlive this; FlushMappings destroys both.
	Lifetime time.Duration
	// ExpireFlows also expires idle established UDP conntrack flows after
	// Lifetime by setting this router namespace's UDP conntrack timeouts, so
	// the whole mapping (not only unsolicited pinholes) has that idle lifetime.
	// Per-network-namespace sysctls only; it affects all UDP flows of this router.
	ExpireFlows bool
}
type Binding struct {
	Inside netip.AddrPort
	// Remote is invalid for EIM; port zero denotes address-dependent mapping.
	Remote  netip.AddrPort
	Outside netip.AddrPort
}
type NAT struct {
	NS       *Namespace
	Config   NATConfig
	Bindings []Binding
	rules    string
}

func (n *Namespace) NAT(cfg NATConfig) (*NAT, error) {
	if cfg.Mapping == "" {
		cfg.Mapping = Kernel
	}
	if cfg.Filtering == "" {
		cfg.Filtering = FilterAddressPortDependent
	}
	if cfg.Allocation == "" {
		cfg.Allocation = Preserve
	}
	if cfg.Lifetime == 0 {
		cfg.Lifetime = time.Minute
	}
	if !cfg.ExternalIP.Is4() || cfg.WAN == "" || cfg.LAN == "" || cfg.Lifetime < time.Second {
		return nil, fmt.Errorf("natlab: IPv4 external address, interfaces and lifetime >=1s required")
	}
	switch cfg.Mapping {
	case Kernel, EndpointIndependent, AddressDependent, AddressPortDependent:
	default:
		return nil, fmt.Errorf("natlab: unknown mapping %q", cfg.Mapping)
	}
	switch cfg.Filtering {
	case FilterEndpointIndependent, FilterAddressDependent, FilterAddressPortDependent:
	default:
		return nil, fmt.Errorf("natlab: unknown filtering %q", cfg.Filtering)
	}
	switch cfg.Allocation {
	case Preserve, Sequential, Random:
	default:
		return nil, fmt.Errorf("natlab: unknown allocation %q", cfg.Allocation)
	}
	if cfg.Mapping == Kernel && (cfg.Filtering != FilterAddressPortDependent || cfg.Hairpin || cfg.Allocation == Sequential) {
		return nil, fmt.Errorf("natlab: kernel mode only promises conntrack filtering, no hairpin or sequential allocator; use fixtures")
	}
	if len(cfg.Inside) > 64 || len(cfg.Destinations) > 64 {
		return nil, fmt.Errorf("natlab: fixture exceeds 64 endpoints/destinations")
	}
	for _, ep := range append(append([]netip.AddrPort{}, cfg.Inside...), cfg.Destinations...) {
		if !ep.Addr().Is4() || ep.Port() == 0 {
			return nil, fmt.Errorf("natlab: fixtures require IPv4 UDP endpoints")
		}
	}
	if cfg.Mapping != Kernel && (len(cfg.Inside) == 0 || len(cfg.Destinations) == 0) {
		return nil, fmt.Errorf("natlab: fixtures require inside endpoints and destinations")
	}
	nat := &NAT{NS: n, Config: cfg}
	if err := nat.allocate(); err != nil {
		return nil, err
	}
	nat.rules = nat.render()
	// Per-network-namespace sysctls, never host sysctls.
	if _, err := n.Run("sysctl", "-qw", "net.ipv4.ip_forward=1", "net.ipv4.conf.all.rp_filter=0", "net.ipv4.conf.default.rp_filter=0"); err != nil {
		return nil, err
	}
	if err := nat.install(""); err != nil {
		return nil, err
	}
	if cfg.ExpireFlows {
		// Conntrack is loaded by the NAT table above; these are per-netns.
		secs := int(cfg.Lifetime.Seconds())
		if _, err := n.Run("sysctl", "-qw", fmt.Sprintf("net.netfilter.nf_conntrack_udp_timeout=%d", secs), fmt.Sprintf("net.netfilter.nf_conntrack_udp_timeout_stream=%d", secs)); err != nil {
			return nil, err
		}
	}
	return nat, nil
}
func (n *NAT) allocate() error {
	if n.Config.Mapping == Kernel {
		return nil
	}
	used := make(map[uint16]bool)
	for _, inside := range n.Config.Inside {
		seen := make(map[netip.AddrPort]bool)
		for _, dest := range n.Config.Destinations {
			remote := dest
			switch n.Config.Mapping {
			case EndpointIndependent:
				remote = netip.AddrPort{}
			case AddressDependent:
				remote = netip.AddrPortFrom(dest.Addr(), 0)
			}
			if seen[remote] {
				continue
			}
			seen[remote] = true
			port := inside.Port()
			if n.Config.Allocation != Preserve {
				port = 40000
			}
			if n.Config.Allocation == Random {
				var b [2]byte
				if _, err := rand.Read(b[:]); err != nil {
					return err
				}
				port = 10000 + binary.BigEndian.Uint16(b[:])%50000
			}
			for used[port] || port == 0 {
				port++
				if port == 0 {
					port = 1024
				}
			}
			used[port] = true
			n.Bindings = append(n.Bindings, Binding{Inside: inside, Remote: remote, Outside: netip.AddrPortFrom(n.Config.ExternalIP, port)})
		}
	}
	return nil
}
func (n *NAT) render() string {
	c := n.Config
	var b strings.Builder
	fmt.Fprintln(&b, "table ip nu_natlab {")
	if c.Mapping != Kernel {
		fmt.Fprintf(&b, "set active { type inet_service; flags dynamic,timeout; timeout %ds; size 4096; }\n", int(c.Lifetime.Seconds()))
		fmt.Fprintf(&b, "set allowed_ip { type inet_service . ipv4_addr; flags dynamic,timeout; timeout %ds; size 4096; }\n", int(c.Lifetime.Seconds()))
		fmt.Fprintf(&b, "set allowed_ep { type inet_service . ipv4_addr . inet_service; flags dynamic,timeout; timeout %ds; size 4096; }\n", int(c.Lifetime.Seconds()))
	}
	fmt.Fprintln(&b, "chain prerouting { type nat hook prerouting priority dstnat; policy accept;")
	if c.Mapping != Kernel {
		for _, m := range n.Bindings {
			prefix := ""
			if !c.Hairpin {
				prefix = fmt.Sprintf("iifname %q ", c.WAN)
			}
			fmt.Fprintf(&b, "%sip daddr %s udp dport @active udp dport %d dnat to %s\n", prefix, c.ExternalIP, m.Outside.Port(), m.Inside)
		}
	}
	fmt.Fprintln(&b, "}")
	fmt.Fprintln(&b, "chain forward { type filter hook forward priority filter; policy drop;")
	if c.BlockUDP {
		fmt.Fprintln(&b, "meta l4proto udp drop")
	}
	if c.Mapping == Kernel {
		fmt.Fprintf(&b, "iifname %q oifname %q accept\nct state established,related accept\n", c.LAN, c.WAN)
	} else {
		for _, m := range n.Bindings {
			fmt.Fprintf(&b, "%s update @active { %d } update @allowed_ip { %d . ip daddr } update @allowed_ep { %d . ip daddr . udp dport } accept\n", n.outboundMatch(m), m.Outside.Port(), m.Outside.Port(), m.Outside.Port())
		}
		// Conntrack replies obey their original mapping; unsolicited inbound packets
		// must pass the explicit learned filter after DNAT.
		fmt.Fprintln(&b, "ct state established,related accept")
		filter := ""
		switch c.Filtering {
		case FilterAddressDependent:
			filter = "ct original proto-dst . ip saddr @allowed_ip "
		case FilterAddressPortDependent:
			filter = "ct original proto-dst . ip saddr . udp sport @allowed_ep "
		}
		fmt.Fprintf(&b, "meta l4proto udp ct status dnat %saccept\n", filter)
	}
	fmt.Fprintln(&b, "}")
	fmt.Fprintln(&b, "chain postrouting { type nat hook postrouting priority srcnat; policy accept;")
	if c.Mapping == Kernel {
		flags := ""
		if c.Allocation == Random {
			flags = " fully-random"
		}
		fmt.Fprintf(&b, "oifname %q masquerade%s\n", c.WAN, flags)
	} else {
		for _, m := range n.Bindings {
			fmt.Fprintf(&b, "%s snat to %s\n", n.outboundMatch(m), m.Outside)
		}
		if c.Hairpin {
			fmt.Fprintf(&b, "iifname %q oifname %q ct status dnat masquerade\n", c.LAN, c.LAN)
		}
	}
	fmt.Fprintln(&b, "}\n}")
	return b.String()
}

// install loads the rendered table: first sets and empty chains, then the rules
// in bounded batches. Large fixtures (many inside x destination bindings) would
// otherwise exceed one netlink batch ("Message too long").
func (n *NAT) install(prefix string) error {
	var skeleton strings.Builder
	var rules []string
	skeleton.WriteString(prefix)
	chain := ""
	for _, line := range strings.Split(n.rules, "\n") {
		switch {
		case strings.HasPrefix(line, "chain "):
			chain = strings.Fields(line)[1]
			skeleton.WriteString(line + "\n")
		case chain != "" && strings.HasPrefix(line, "}"):
			chain = ""
			skeleton.WriteString(line + "\n")
		case chain != "" && strings.TrimSpace(line) != "":
			rules = append(rules, "add rule ip nu_natlab "+chain+" "+line)
		default:
			skeleton.WriteString(line + "\n")
		}
	}
	if err := n.NS.NFT(skeleton.String()); err != nil {
		return err
	}
	for len(rules) > 0 {
		batch := rules[:min(64, len(rules))]
		rules = rules[len(batch):]
		if err := n.NS.NFT(strings.Join(batch, "\n") + "\n"); err != nil {
			return err
		}
	}
	return nil
}
func (n *NAT) outboundMatch(m Binding) string {
	match := fmt.Sprintf("iifname %q oifname %q ip saddr %s udp sport %d", n.Config.LAN, n.Config.WAN, m.Inside.Addr(), m.Inside.Port())
	if m.Remote.IsValid() {
		match += fmt.Sprintf(" ip daddr %s", m.Remote.Addr())
		if m.Remote.Port() != 0 {
			match += fmt.Sprintf(" udp dport %d", m.Remote.Port())
		}
	}
	return match
}

// FlushMappings removes conntrack state and learned fixture pinholes. Unlike
// deleting nft rules alone, this really forces existing UDP flows to rebind.
func (n *NAT) FlushMappings() error {
	if err := n.NS.flushConntrack(); err != nil {
		return err
	}
	if n.Config.Mapping != Kernel {
		return n.NS.NFT("flush set ip nu_natlab active\nflush set ip nu_natlab allowed_ip\nflush set ip nu_natlab allowed_ep\n")
	}
	return nil
}

// Rebind replaces the external IP in the NAT rules, then flushes conntrack.
// Assign the new address to the WAN separately through Namespace.Address.
func (n *NAT) Rebind(ip netip.Addr) error {
	if !ip.Is4() {
		return fmt.Errorf("natlab: IPv4 required")
	}
	n.Config.ExternalIP = ip
	for i := range n.Bindings {
		n.Bindings[i].Outside = netip.AddrPortFrom(ip, n.Bindings[i].Outside.Port())
	}
	n.rules = n.render()
	if err := n.install("delete table ip nu_natlab\n"); err != nil {
		return err
	}
	return n.FlushMappings()
}
