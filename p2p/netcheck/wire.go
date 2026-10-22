package netcheck

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

const (
	// ProtocolID is the libp2p stream protocol returning an observer descriptor.
	ProtocolID = "/nu/netcheck/1"
	// ProfileProtocolID exchanges compact NAT profiles between Nu peers.
	ProfileProtocolID = "/nu/natprofile/1"

	// PacketSize is the fixed size of every UDP probe packet in either direction.
	PacketSize   = 64
	endpointSize = 19
	maxSets      = 2
)

var ErrPacket = errors.New("netcheck: malformed packet")

// packet magic: first byte has neither the QUIC fixed bit (0x40) nor the
// magicsock disco envelope prefix, followed by "nc" and the wire version.
var packetMagic = [4]byte{0x02, 'n', 'c', 1}

const (
	typeRequest  = 1
	typeResponse = 2
	typeHairpin  = 3
)

// Request flags (RFC 5780 CHANGE-REQUEST semantics). At most one may be set.
const (
	FlagAltPort = 1 << 0 // reply from the observer's alternate port, same IP
	FlagAltIP   = 1 << 1 // reply from the observer's alternate IP, same port
)

// Role identifies which observer socket an endpoint belongs to.
type Role uint8

const (
	RolePrimary Role = 1
	RoleAltPort Role = 2
	RoleAltIP   Role = 3
)

// packet is the fixed 64-byte probe. Offsets:
//
//	0-3   magic 02 6e 63 01
//	4     type: 1 request, 2 response, 3 hairpin (client-to-client only)
//	5     request flags (FlagAltPort|FlagAltIP, at most one); zero otherwise
//	6-7   request target port (0 = reply to source port); zero otherwise
//	8-23  transaction ID (random)
//	24-39 cookie: request (zero on plain requests); response: fresh cookie
//	      bound to the request source IP
//	40-58 response: observed request source endpoint; zero otherwise
//	59    response: Role of the replying socket; zero otherwise
//	60-63 reserved, zero
type packet struct {
	Type       uint8
	Flags      uint8
	TargetPort uint16
	TxID       [16]byte
	Cookie     [16]byte
	Observed   netip.AddrPort
	Role       Role
}

func (p packet) marshal() [PacketSize]byte {
	var b [PacketSize]byte
	copy(b[:], packetMagic[:])
	b[4], b[5] = p.Type, p.Flags
	binary.BigEndian.PutUint16(b[6:], p.TargetPort)
	copy(b[8:], p.TxID[:])
	copy(b[24:], p.Cookie[:])
	if p.Observed.IsValid() {
		putEndpoint(b[40:59], p.Observed)
	}
	b[59] = byte(p.Role)
	return b
}

func parsePacket(b []byte) (packet, error) {
	if len(b) != PacketSize || [4]byte(b[:4]) != packetMagic {
		return packet{}, ErrPacket
	}
	p := packet{Type: b[4], Flags: b[5], TargetPort: binary.BigEndian.Uint16(b[6:]), Role: Role(b[59])}
	copy(p.TxID[:], b[8:24])
	copy(p.Cookie[:], b[24:40])
	if [4]byte(b[60:64]) != ([4]byte{}) {
		return packet{}, ErrPacket
	}
	var err error
	zeroEndpoint := [endpointSize]byte(b[40:59]) == [endpointSize]byte{}
	switch p.Type {
	case typeRequest:
		if p.Flags&^(FlagAltPort|FlagAltIP) != 0 || p.Flags == FlagAltPort|FlagAltIP || !zeroEndpoint || p.Role != 0 {
			return packet{}, ErrPacket
		}
	case typeResponse:
		if p.Flags != 0 || p.TargetPort != 0 || p.Role < RolePrimary || p.Role > RoleAltIP {
			return packet{}, ErrPacket
		}
		if p.Observed, err = getEndpoint(b[40:59]); err != nil {
			return packet{}, err
		}
	case typeHairpin:
		if p.Flags != 0 || p.TargetPort != 0 || p.Cookie != ([16]byte{}) || !zeroEndpoint || p.Role != 0 {
			return packet{}, ErrPacket
		}
	default:
		return packet{}, ErrPacket
	}
	return p, nil
}

// Endpoint encoding shared with /nu/path/1: family:u8 (4|6), address:16
// (IPv4 as 12 zero bytes + 4 bytes), port:u16.
func putEndpoint(b []byte, ep netip.AddrPort) {
	a := ep.Addr()
	if a.Is4() {
		b[0] = 4
		v4 := a.As4()
		copy(b[13:17], v4[:])
	} else {
		b[0] = 6
		v6 := a.As16()
		copy(b[1:17], v6[:])
	}
	binary.BigEndian.PutUint16(b[17:], ep.Port())
}
func getEndpoint(b []byte) (netip.AddrPort, error) {
	var a netip.Addr
	switch b[0] {
	case 4:
		if [12]byte(b[1:13]) != ([12]byte{}) {
			return netip.AddrPort{}, ErrPacket
		}
		a = netip.AddrFrom4([4]byte(b[13:17]))
	case 6:
		a = netip.AddrFrom16([16]byte(b[1:17]))
		if a.Is4In6() {
			return netip.AddrPort{}, ErrPacket
		}
	default:
		return netip.AddrPort{}, ErrPacket
	}
	ep := netip.AddrPortFrom(a, binary.BigEndian.Uint16(b[17:]))
	if !usable(ep) {
		return netip.AddrPort{}, ErrPacket
	}
	return ep, nil
}

// usable rejects unspecified, multicast, broadcast, zoned and zero-port
// endpoints. Loopback/private literals remain legal on the wire; deployment
// policy (not the codec) decides which observers to use.
func usable(ep netip.AddrPort) bool {
	a := ep.Addr()
	return ep.IsValid() && ep.Port() != 0 && a.Zone() == "" && !a.IsUnspecified() && !a.IsMulticast() &&
		!a.IsLinkLocalUnicast() && a != netip.AddrFrom4([4]byte{255, 255, 255, 255}) && !a.Is4In6()
}

// EndpointSet is one observer's sockets in one address family. AltPort shares
// Primary's IP; AltIP shares Primary's port on a different IP. Either may be absent.
type EndpointSet struct {
	Primary netip.AddrPort `json:"primary"`
	AltPort netip.AddrPort `json:"alt_port,omitzero"`
	AltIP   netip.AddrPort `json:"alt_ip,omitzero"`
}

func (s EndpointSet) Validate() error {
	p := s.Primary
	switch {
	case !usable(p):
		return fmt.Errorf("netcheck: invalid primary endpoint %s", p)
	case s.AltPort.IsValid() && (!usable(s.AltPort) || s.AltPort.Addr() != p.Addr() || s.AltPort.Port() == p.Port()):
		return fmt.Errorf("netcheck: alternate port %s must share the primary IP with a different port", s.AltPort)
	case s.AltIP.IsValid() && (!usable(s.AltIP) || s.AltIP.Addr().Is4() != p.Addr().Is4() || s.AltIP.Addr() == p.Addr() || s.AltIP.Port() != p.Port()):
		return fmt.Errorf("netcheck: alternate IP %s must be another same-family IP on the primary port", s.AltIP)
	}
	return nil
}
func (s EndpointSet) endpoint(r Role) netip.AddrPort {
	switch r {
	case RolePrimary:
		return s.Primary
	case RoleAltPort:
		return s.AltPort
	case RoleAltIP:
		return s.AltIP
	}
	return netip.AddrPort{}
}

// Descriptor is the /nu/netcheck/1 response: at most one EndpointSet per family.
// Encoding: version=1:u8, count:u8 (1..6), then count × (role:u8, endpoint:19).
type Descriptor struct{ Sets []EndpointSet }

func (d Descriptor) Validate() error {
	if len(d.Sets) == 0 || len(d.Sets) > maxSets {
		return fmt.Errorf("netcheck: descriptor needs 1-%d endpoint sets", maxSets)
	}
	families := map[bool]bool{}
	for _, s := range d.Sets {
		if err := s.Validate(); err != nil {
			return err
		}
		if families[s.Primary.Addr().Is4()] {
			return errors.New("netcheck: one endpoint set per address family")
		}
		families[s.Primary.Addr().Is4()] = true
	}
	return nil
}
func (d Descriptor) MarshalBinary() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	b := []byte{1, 0}
	for _, s := range d.Sets {
		for r := RolePrimary; r <= RoleAltIP; r++ {
			if ep := s.endpoint(r); ep.IsValid() {
				var e [endpointSize]byte
				putEndpoint(e[:], ep)
				b = append(append(b, byte(r)), e[:]...)
				b[1]++
			}
		}
	}
	return b, nil
}
func DecodeDescriptor(b []byte) (Descriptor, error) {
	if len(b) < 2 || b[0] != 1 || b[1] == 0 || b[1] > 3*maxSets || len(b) != 2+int(b[1])*(1+endpointSize) {
		return Descriptor{}, fmt.Errorf("%w: descriptor framing", ErrPacket)
	}
	var d Descriptor
	index := map[bool]int{}
	for i := 0; i < int(b[1]); i++ {
		e := b[2+i*(1+endpointSize):]
		role := Role(e[0])
		ep, err := getEndpoint(e[1 : 1+endpointSize])
		if err != nil || role < RolePrimary || role > RoleAltIP {
			return Descriptor{}, fmt.Errorf("%w: descriptor entry", ErrPacket)
		}
		v4 := ep.Addr().Is4()
		j, ok := index[v4]
		if !ok {
			if role != RolePrimary {
				return Descriptor{}, fmt.Errorf("%w: primary must precede alternates", ErrPacket)
			}
			index[v4] = len(d.Sets)
			d.Sets = append(d.Sets, EndpointSet{Primary: ep})
			continue
		}
		if j != len(d.Sets)-1 {
			return Descriptor{}, fmt.Errorf("%w: endpoint sets must be contiguous", ErrPacket)
		}
		s := &d.Sets[j]
		switch {
		case role == RoleAltPort && !s.AltPort.IsValid() && !s.AltIP.IsValid():
			s.AltPort = ep
		case role == RoleAltIP && !s.AltIP.IsValid():
			s.AltIP = ep
		default:
			return Descriptor{}, fmt.Errorf("%w: duplicate or misordered role", ErrPacket)
		}
	}
	if err := d.Validate(); err != nil {
		return Descriptor{}, fmt.Errorf("%w: %v", ErrPacket, err)
	}
	return d, nil
}
