package netcheck

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// ProfileVersion is the only profile encoding this package emits/accepts.
const (
	ProfileVersion = 1
	ProfileSize    = 18
)

var ErrProfile = errors.New("netcheck: invalid NAT profile")

// Every enum reserves zero for "not measured". Wire values are frozen for v1.
type (
	UDPStatus  uint8
	Mapping    uint8
	Filtering  uint8
	Allocation uint8
	Hairpin    uint8
	IPv6Status uint8
	Bound      uint8
	PortMaps   uint8
)

const (
	UDPUnknown UDPStatus = iota
	UDPOK
	UDPBlocked
)
const (
	MappingUnknown Mapping = iota
	MappingNone            // no translation: observed == local endpoint
	MappingEndpointIndependent
	MappingAddressDependent
	MappingAddressPortDependent
	MappingEndpointDependent // dependent; address vs port not resolvable by the observers
)
const (
	FilterUnknown Filtering = iota
	FilterEndpointIndependent
	FilterAddressDependent
	FilterAddressPortDependent
)
const (
	AllocUnknown Allocation = iota
	AllocPreserving
	AllocSequential
	AllocRandom
)
const (
	HairpinUnknown Hairpin = iota
	HairpinYes
	HairpinNo
)
const (
	IPv6Unknown     IPv6Status = iota
	IPv6Unavailable            // no usable global IPv6 socket/route
	IPv6Blocked                // IPv6 UDP sends work but no observer answered
	IPv6OK
)
const (
	BoundExact   Bound = iota // estimate of a measured bracket
	BoundAtLeast              // never expired up to the longest probe
	BoundBelow                // already expired at the shortest probe
)
const (
	PortMapPCP     PortMaps = 1 << 0
	PortMapNATPMP  PortMaps = 1 << 1
	PortMapUPnP    PortMaps = 1 << 2
	PortMapChecked PortMaps = 1 << 7 // probe ran; with no protocol bits: none found
)

const (
	flagLifetimeAtLeast = 1 << 0
	flagLifetimeBelow   = 1 << 1
	flagVaryingIP       = 1 << 2
)

// Profile is the compact, address-free NAT profile exchanged with peers.
// v1 layout (18 bytes, big-endian):
//
//	0 version=1 | 1 udp4 | 2 mapping4 | 3 filtering4 | 4 allocation4 |
//	5-6 delta int16 | 7-8 jitter uint16 | 9-10 lifetime seconds uint16 |
//	11 flags (bit0 lifetime>=, bit1 lifetime<, bit2 varying external IP) |
//	12 hairpin | 13 ipv6 | 14 mapping6 | 15 filtering6 | 16 portmap | 17 observers
type Profile struct {
	UDP           UDPStatus  `json:"udp"`
	Mapping       Mapping    `json:"mapping"`
	Filtering     Filtering  `json:"filtering"`
	Allocation    Allocation `json:"allocation"`
	Delta         int16      `json:"delta,omitempty"`
	Jitter        uint16     `json:"jitter,omitempty"`
	Lifetime      uint16     `json:"lifetime_seconds,omitempty"` // 0: not measured
	LifetimeBound Bound      `json:"lifetime_bound,omitempty"`
	VaryingIP     bool       `json:"varying_ip,omitempty"`
	Hairpin       Hairpin    `json:"hairpin"`
	IPv6          IPv6Status `json:"ipv6"`
	Mapping6      Mapping    `json:"mapping6"`
	Filtering6    Filtering  `json:"filtering6"`
	PortMap       PortMaps   `json:"portmap"`
	Observers     uint8      `json:"observers"` // distinct observer IPs that answered
}

func (p Profile) Validate() error {
	bad := func(what string) error { return fmt.Errorf("%w: %s", ErrProfile, what) }
	switch {
	case p.UDP > UDPBlocked, p.Mapping > MappingEndpointDependent, p.Filtering > FilterAddressPortDependent,
		p.Allocation > AllocRandom, p.Hairpin > HairpinNo, p.IPv6 > IPv6OK, p.Mapping6 > MappingEndpointDependent,
		p.Filtering6 > FilterAddressPortDependent, p.LifetimeBound > BoundBelow:
		return bad("enum out of range")
	case p.PortMap&^(PortMapPCP|PortMapNATPMP|PortMapUPnP|PortMapChecked) != 0, p.PortMap != 0 && p.PortMap&PortMapChecked == 0:
		return bad("port mapping bits")
	case (p.Allocation == AllocSequential) != (p.Delta != 0), p.Allocation != AllocSequential && p.Jitter != 0:
		return bad("delta/jitter only for sequential allocation")
	case p.Lifetime == 0 && p.LifetimeBound != BoundExact:
		return bad("lifetime bound without lifetime")
	case p.UDP != UDPOK && (p.Mapping|Mapping(p.Filtering)|Mapping(p.Allocation)|Mapping(p.Hairpin) != 0 || p.Lifetime != 0 || p.VaryingIP):
		return bad("IPv4 results without working UDP")
	case p.IPv6 != IPv6OK && p.Mapping6|Mapping(p.Filtering6) != 0:
		return bad("IPv6 results without working IPv6")
	}
	return nil
}

// AppendBinary appends the canonical v1 encoding. Invalid profiles are refused.
func (p Profile) AppendBinary(b []byte) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return b, err
	}
	var flags byte
	switch p.LifetimeBound {
	case BoundAtLeast:
		flags |= flagLifetimeAtLeast
	case BoundBelow:
		flags |= flagLifetimeBelow
	}
	if p.VaryingIP {
		flags |= flagVaryingIP
	}
	b = append(b, ProfileVersion, byte(p.UDP), byte(p.Mapping), byte(p.Filtering), byte(p.Allocation))
	b = binary.BigEndian.AppendUint16(b, uint16(p.Delta))
	b = binary.BigEndian.AppendUint16(b, p.Jitter)
	b = binary.BigEndian.AppendUint16(b, p.Lifetime)
	return append(b, flags, byte(p.Hairpin), byte(p.IPv6), byte(p.Mapping6), byte(p.Filtering6), byte(p.PortMap), p.Observers), nil
}
func (p Profile) MarshalBinary() ([]byte, error) { return p.AppendBinary(make([]byte, 0, ProfileSize)) }

// DecodeProfile accepts exactly one canonical v1 encoding; other versions,
// lengths, reserved bits and inconsistent fields are errors (never guessed).
func DecodeProfile(b []byte) (Profile, error) {
	if len(b) == 0 || b[0] != ProfileVersion {
		return Profile{}, fmt.Errorf("%w: unsupported version", ErrProfile)
	}
	if len(b) != ProfileSize {
		return Profile{}, fmt.Errorf("%w: length %d", ErrProfile, len(b))
	}
	flags := b[11]
	if flags&^(flagLifetimeAtLeast|flagLifetimeBelow|flagVaryingIP) != 0 || flags&(flagLifetimeAtLeast|flagLifetimeBelow) == flagLifetimeAtLeast|flagLifetimeBelow {
		return Profile{}, fmt.Errorf("%w: flags", ErrProfile)
	}
	p := Profile{
		UDP: UDPStatus(b[1]), Mapping: Mapping(b[2]), Filtering: Filtering(b[3]), Allocation: Allocation(b[4]),
		Delta: int16(binary.BigEndian.Uint16(b[5:])), Jitter: binary.BigEndian.Uint16(b[7:]), Lifetime: binary.BigEndian.Uint16(b[9:]),
		VaryingIP: flags&flagVaryingIP != 0, Hairpin: Hairpin(b[12]), IPv6: IPv6Status(b[13]), Mapping6: Mapping(b[14]),
		Filtering6: Filtering(b[15]), PortMap: PortMaps(b[16]), Observers: b[17],
	}
	if flags&flagLifetimeAtLeast != 0 {
		p.LifetimeBound = BoundAtLeast
	} else if flags&flagLifetimeBelow != 0 {
		p.LifetimeBound = BoundBelow
	}
	return p, p.Validate()
}
func (p *Profile) UnmarshalBinary(b []byte) error {
	v, err := DecodeProfile(b)
	if err == nil {
		*p = v
	}
	return err
}

var (
	udpNames     = []string{"unknown", "ok", "blocked"}
	mappingNames = []string{"unknown", "none", "eim", "adm", "apdm", "edm"}
	filterNames  = []string{"unknown", "eif", "adf", "apdf"}
	allocNames   = []string{"unknown", "preserving", "sequential", "random"}
	hairpinNames = []string{"unknown", "yes", "no"}
	ipv6Names    = []string{"unknown", "unavailable", "blocked", "ok"}
	boundNames   = []string{"exact", "at-least", "below"}
)

func name(names []string, v uint8) string {
	if int(v) < len(names) {
		return names[v]
	}
	return fmt.Sprintf("invalid(%d)", v)
}
func parseName(names []string, s string) (uint8, error) {
	for i, n := range names {
		if n == s {
			return uint8(i), nil
		}
	}
	return 0, fmt.Errorf("%w: unknown name %q", ErrProfile, s)
}

func (v UDPStatus) String() string  { return name(udpNames, uint8(v)) }
func (v Mapping) String() string    { return name(mappingNames, uint8(v)) }
func (v Filtering) String() string  { return name(filterNames, uint8(v)) }
func (v Allocation) String() string { return name(allocNames, uint8(v)) }
func (v Hairpin) String() string    { return name(hairpinNames, uint8(v)) }
func (v IPv6Status) String() string { return name(ipv6Names, uint8(v)) }
func (v Bound) String() string      { return name(boundNames, uint8(v)) }
func (v PortMaps) String() string {
	if v&PortMapChecked == 0 {
		return "unknown"
	}
	var out []string
	for _, p := range []struct {
		bit  PortMaps
		name string
	}{{PortMapPCP, "pcp"}, {PortMapNATPMP, "nat-pmp"}, {PortMapUPnP, "upnp"}} {
		if v&p.bit != 0 {
			out = append(out, p.name)
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, "+")
}

func (v UDPStatus) MarshalText() ([]byte, error)  { return []byte(v.String()), nil }
func (v Mapping) MarshalText() ([]byte, error)    { return []byte(v.String()), nil }
func (v Filtering) MarshalText() ([]byte, error)  { return []byte(v.String()), nil }
func (v Allocation) MarshalText() ([]byte, error) { return []byte(v.String()), nil }
func (v Hairpin) MarshalText() ([]byte, error)    { return []byte(v.String()), nil }
func (v IPv6Status) MarshalText() ([]byte, error) { return []byte(v.String()), nil }
func (v Bound) MarshalText() ([]byte, error)      { return []byte(v.String()), nil }
func (v PortMaps) MarshalText() ([]byte, error)   { return []byte(v.String()), nil }

func (v *UDPStatus) UnmarshalText(b []byte) error {
	n, err := parseName(udpNames, string(b))
	*v = UDPStatus(n)
	return err
}
func (v *Mapping) UnmarshalText(b []byte) error {
	n, err := parseName(mappingNames, string(b))
	*v = Mapping(n)
	return err
}
func (v *Filtering) UnmarshalText(b []byte) error {
	n, err := parseName(filterNames, string(b))
	*v = Filtering(n)
	return err
}
func (v *Allocation) UnmarshalText(b []byte) error {
	n, err := parseName(allocNames, string(b))
	*v = Allocation(n)
	return err
}
func (v *Hairpin) UnmarshalText(b []byte) error {
	n, err := parseName(hairpinNames, string(b))
	*v = Hairpin(n)
	return err
}
func (v *IPv6Status) UnmarshalText(b []byte) error {
	n, err := parseName(ipv6Names, string(b))
	*v = IPv6Status(n)
	return err
}
func (v *Bound) UnmarshalText(b []byte) error {
	n, err := parseName(boundNames, string(b))
	*v = Bound(n)
	return err
}
func (v *PortMaps) UnmarshalText(b []byte) error {
	s := string(b)
	switch s {
	case "unknown":
		*v = 0
		return nil
	case "none":
		*v = PortMapChecked
		return nil
	}
	out := PortMapChecked
	for _, part := range strings.Split(s, "+") {
		switch part {
		case "pcp":
			out |= PortMapPCP
		case "nat-pmp":
			out |= PortMapNATPMP
		case "upnp":
			out |= PortMapUPnP
		default:
			return fmt.Errorf("%w: port mapping %q", ErrProfile, part)
		}
	}
	*v = out
	return nil
}

// String is the one-line `nu doctor` form, e.g.
// "udp=ok mapping=apdm filtering=apdf allocation=sequential(delta=+1,jitter=0)
// lifetime=30s hairpin=no ipv6=unavailable portmap=none observers=2".
func (p Profile) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "udp=%s mapping=%s filtering=%s allocation=%s", p.UDP, p.Mapping, p.Filtering, p.Allocation)
	if p.Allocation == AllocSequential {
		fmt.Fprintf(&b, "(delta=%+d,jitter=%d)", p.Delta, p.Jitter)
	}
	switch {
	case p.Lifetime == 0:
		b.WriteString(" lifetime=unknown")
	case p.LifetimeBound == BoundAtLeast:
		fmt.Fprintf(&b, " lifetime>=%ds", p.Lifetime)
	case p.LifetimeBound == BoundBelow:
		fmt.Fprintf(&b, " lifetime<%ds", p.Lifetime)
	default:
		fmt.Fprintf(&b, " lifetime=%ds", p.Lifetime)
	}
	if p.VaryingIP {
		b.WriteString(" varying-ip")
	}
	fmt.Fprintf(&b, " hairpin=%s ipv6=%s", p.Hairpin, p.IPv6)
	if p.IPv6 == IPv6OK {
		fmt.Fprintf(&b, "(mapping=%s,filtering=%s)", p.Mapping6, p.Filtering6)
	}
	fmt.Fprintf(&b, " portmap=%s observers=%d", p.PortMap, p.Observers)
	return b.String()
}
