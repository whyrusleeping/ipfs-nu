package netcheck

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/netmon"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// Frozen v1 vectors: changing any of these is a wire break and needs v2.
var profileVectors = []struct {
	hex  string
	p    Profile
	text string
}{
	{"010000000000000000000000000000000000", Profile{},
		"udp=unknown mapping=unknown filtering=unknown allocation=unknown lifetime=unknown hairpin=unknown ipv6=unknown portmap=unknown observers=0"},
	{"010200000000000000000000000000000002", Profile{UDP: UDPBlocked, Observers: 2},
		"udp=blocked mapping=unknown filtering=unknown allocation=unknown lifetime=unknown hairpin=unknown ipv6=unknown portmap=unknown observers=2"},
	{"010102010100000000001e00010301018103",
		Profile{UDP: UDPOK, Mapping: MappingEndpointIndependent, Filtering: FilterEndpointIndependent, Allocation: AllocPreserving, Lifetime: 30, Hairpin: HairpinYes, IPv6: IPv6OK, Mapping6: MappingNone, Filtering6: FilterEndpointIndependent, PortMap: PortMapChecked | PortMapPCP, Observers: 3},
		"udp=ok mapping=eim filtering=eif allocation=preserving lifetime=30s hairpin=yes ipv6=ok(mapping=none,filtering=eif) portmap=pcp observers=3"},
	{"01010403020007000300f005020000008003",
		Profile{UDP: UDPOK, Mapping: MappingAddressPortDependent, Filtering: FilterAddressPortDependent, Allocation: AllocSequential, Delta: 7, Jitter: 3, Lifetime: 240, LifetimeBound: BoundAtLeast, VaryingIP: true, Hairpin: HairpinNo, PortMap: PortMapChecked, Observers: 3},
		"udp=ok mapping=apdm filtering=apdf allocation=sequential(delta=+7,jitter=3) lifetime>=240s varying-ip hairpin=no ipv6=unknown portmap=none observers=3"},
	{"0101030202fffe00000005020002000000ff",
		Profile{UDP: UDPOK, Mapping: MappingAddressDependent, Filtering: FilterAddressDependent, Allocation: AllocSequential, Delta: -2, Lifetime: 5, LifetimeBound: BoundBelow, IPv6: IPv6Blocked, Observers: 255},
		"udp=ok mapping=adm filtering=adf allocation=sequential(delta=-2,jitter=0) lifetime<5s hairpin=unknown ipv6=blocked portmap=unknown observers=255"},
}

func TestProfileVectors(t *testing.T) {
	for _, v := range profileVectors {
		want, _ := hex.DecodeString(v.hex)
		got, err := v.p.MarshalBinary()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("encode %+v: %x %v, want %s", v.p, got, err, v.hex)
		}
		p, err := DecodeProfile(want)
		if err != nil || p != v.p {
			t.Fatalf("decode %s: %+v %v", v.hex, p, err)
		}
		if p.String() != v.text {
			t.Fatalf("text:\n%s\n%s", p.String(), v.text)
		}
		j, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var back Profile
		if err := json.Unmarshal(j, &back); err != nil || back != p {
			t.Fatalf("json %s: %+v %v", j, back, err)
		}
	}
	base, _ := hex.DecodeString(profileVectors[2].hex) // udp ok, eim, preserving, 30s
	for _, bad := range []struct {
		what       string
		index      int
		value      byte
		truncate   int
		appendByte bool
	}{
		{what: "version", index: 0, value: 2}, {what: "short", truncate: 17}, {what: "trailing", appendByte: true},
		{what: "udp enum", index: 1, value: 3}, {what: "mapping enum", index: 2, value: 6},
		{what: "filtering enum", index: 3, value: 4}, {what: "allocation enum", index: 4, value: 4},
		{what: "delta without sequential", index: 6, value: 1}, {what: "jitter without sequential", index: 8, value: 1},
		{what: "sequential without delta", index: 4, value: 2}, {what: "both bounds", index: 11, value: 3},
		{what: "reserved flag", index: 11, value: 8}, {what: "hairpin enum", index: 12, value: 3},
		{what: "ipv6 enum", index: 13, value: 4}, {what: "portmap reserved", index: 16, value: 0x89},
		{what: "portmap unchecked", index: 16, value: 0x01},
	} {
		b := append([]byte(nil), base...)
		switch {
		case bad.truncate > 0:
			b = b[:bad.truncate]
		case bad.appendByte:
			b = append(b, 0)
		default:
			b[bad.index] = bad.value
		}
		if _, err := DecodeProfile(b); !errors.Is(err, ErrProfile) {
			t.Fatalf("accepted %s: %x", bad.what, b)
		}
	}
	for _, bad := range []Profile{
		{Lifetime: 0, LifetimeBound: BoundAtLeast},
		{Mapping: MappingEndpointIndependent},
		{UDP: UDPBlocked, Lifetime: 5},
		{IPv6: IPv6Blocked, Filtering6: FilterAddressDependent},
	} {
		if _, err := bad.MarshalBinary(); !errors.Is(err, ErrProfile) {
			t.Fatalf("encoded inconsistent %+v", bad)
		}
	}
}

func FuzzDecodeProfile(f *testing.F) {
	for _, v := range profileVectors {
		b, _ := hex.DecodeString(v.hex)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := DecodeProfile(b)
		if err != nil {
			return
		}
		out, err := p.MarshalBinary()
		if err != nil || !bytes.Equal(out, b) {
			t.Fatalf("non-canonical %x -> %x %v", b, out, err)
		}
	})
}

func FuzzParsePacket(f *testing.F) {
	req := packet{Type: typeRequest, Flags: FlagAltPort, TargetPort: 4000}
	resp := packet{Type: typeResponse, Observed: netip.MustParseAddrPort("198.51.100.7:4242"), Role: RoleAltIP}
	for _, p := range []packet{req, resp, {Type: typeHairpin}} {
		b := p.marshal()
		f.Add(b[:])
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := parsePacket(b)
		if err != nil {
			return
		}
		if out := p.marshal(); !bytes.Equal(out[:], b) {
			t.Fatalf("non-canonical packet %x -> %x", b, out)
		}
	})
}

func FuzzDecodeDescriptor(f *testing.F) {
	d := Descriptor{Sets: []EndpointSet{
		{Primary: netip.MustParseAddrPort("198.51.100.1:3478"), AltPort: netip.MustParseAddrPort("198.51.100.1:3479"), AltIP: netip.MustParseAddrPort("198.51.100.2:3478")},
		{Primary: netip.MustParseAddrPort("[2001:db8::1]:3478")},
	}}
	b, err := d.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) {
		d, err := DecodeDescriptor(b)
		if err != nil {
			return
		}
		out, err := d.MarshalBinary()
		if err != nil || !bytes.Equal(out, b) {
			t.Fatalf("non-canonical descriptor %x -> %x %v", b, out, err)
		}
	})
}

// A wire vector for the probe packet and descriptor.
func TestWireVectors(t *testing.T) {
	p := packet{Type: typeResponse, Observed: netip.MustParseAddrPort("198.51.100.7:4242"), Role: RoleAltPort}
	p.TxID[0], p.Cookie[15] = 0xaa, 0xbb
	b := p.marshal()
	want := "026e6301020000" + "00" + "aa" + "000000000000000000000000000000" + "000000000000000000000000000000bb" +
		"04" + "000000000000000000000000" + "c6336407" + "1092" + "02" + "00000000"
	if hex.EncodeToString(b[:]) != want {
		t.Fatalf("packet %x", b)
	}
	d := Descriptor{Sets: []EndpointSet{{Primary: netip.MustParseAddrPort("198.51.100.1:3478"), AltPort: netip.MustParseAddrPort("198.51.100.1:3479")}}}
	db, _ := d.MarshalBinary()
	if hex.EncodeToString(db) != "010201"+"04000000000000000000000000c63364010d96"+"02"+"04000000000000000000000000c63364010d97" {
		t.Fatalf("descriptor %x", db)
	}
}

func loopbackServer(t *testing.T, sets ...ServerSet) *Server {
	t.Helper()
	s, err := NewServer(ServerConfig{Sets: sets})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func freePort(t *testing.T) uint16 {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return uint16(pc.LocalAddr().(*net.UDPAddr).Port)
}

// Loopback has no NAT: mapping none, endpoint-independent filtering, hairpin,
// and a lifetime that outlasts the (shortened) ladder.
func TestLoopbackProfile(t *testing.T) {
	p1, p2, p3 := freePort(t), freePort(t), freePort(t)
	ep := func(ip string, port uint16) netip.AddrPort { return netip.AddrPortFrom(netip.MustParseAddr(ip), port) }
	s1 := loopbackServer(t, ServerSet{Listen: EndpointSet{Primary: ep("127.0.0.1", p1), AltPort: ep("127.0.0.1", p2), AltIP: ep("127.0.0.2", p1)}})
	s2 := loopbackServer(t, ServerSet{Listen: EndpointSet{Primary: ep("127.0.0.3", p3), AltPort: ep("127.0.0.3", p1)}})
	obs := append(Observers("one", s1.Descriptor()), Observers("two", s2.Descriptor())...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := Config{Timeout: 300 * time.Millisecond, Retransmit: 100 * time.Millisecond, LifetimeMin: 100 * time.Millisecond, LifetimeMax: 200 * time.Millisecond,
		PortMap: func(context.Context) PortMaps { return 0 }}
	r, err := Run(ctx, cfg, obs)
	if err != nil {
		t.Fatal(err)
	}
	p := r.Profile
	if p.UDP != UDPOK || p.Mapping != MappingNone || p.Filtering != FilterEndpointIndependent || p.Hairpin != HairpinYes ||
		p.LifetimeBound != BoundAtLeast || p.Observers != 3 || p.PortMap != PortMapChecked || p.IPv6 != IPv6Unknown {
		b, _ := json.MarshalIndent(r, "", " ")
		t.Fatalf("%s\n%s", p, b)
	}
}

func TestServerAuthorization(t *testing.T) {
	port := freePort(t)
	set := EndpointSet{Primary: netip.MustParseAddrPort("127.0.0.1:" + itoa(port)), AltPort: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), freePort(t))}
	s := loopbackServer(t, ServerSet{Listen: set})
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	other, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2)})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	exchange := func(c *net.UDPConn, p packet) (packet, netip.AddrPort, bool) {
		b := p.marshal()
		if _, err := c.WriteToUDPAddrPort(b[:], set.Primary); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		buf := make([]byte, 128)
		n, from, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			return packet{}, from, false
		}
		if n != PacketSize {
			t.Fatalf("amplification/size: %d", n)
		}
		r, err := parsePacket(buf[:n])
		return r, from, err == nil
	}
	if _, _, ok := exchange(client, packet{Type: typeRequest, Flags: FlagAltPort}); ok {
		t.Fatal("change request without cookie answered")
	}
	plain, from, ok := exchange(client, packet{Type: typeRequest})
	if !ok || from != set.Primary || plain.Observed != client.LocalAddr().(*net.UDPAddr).AddrPort() || plain.Role != RolePrimary {
		t.Fatalf("plain: %+v from %s %v", plain, from, ok)
	}
	if r, from, ok := exchange(client, packet{Type: typeRequest, Flags: FlagAltPort, Cookie: plain.Cookie}); !ok || from != set.AltPort || r.Role != RoleAltPort {
		t.Fatalf("alt port: %+v from %s %v", r, from, ok)
	}
	if _, _, ok := exchange(client, packet{Type: typeRequest, Flags: FlagAltIP, Cookie: plain.Cookie}); ok {
		t.Fatal("alt IP answered without an alt-IP socket")
	}
	if _, _, ok := exchange(other, packet{Type: typeRequest, Flags: FlagAltPort, Cookie: plain.Cookie}); ok {
		t.Fatal("cookie accepted from another source IP")
	}
	// Redirects go only to the requester's own IP.
	target, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	redirect := packet{Type: typeRequest, TargetPort: uint16(target.LocalAddr().(*net.UDPAddr).Port), Cookie: plain.Cookie}
	b := redirect.marshal()
	if _, err := client.WriteToUDPAddrPort(b[:], set.Primary); err != nil {
		t.Fatal(err)
	}
	_ = target.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 128)
	if n, _, err := target.ReadFromUDPAddrPort(buf); err != nil || n != PacketSize {
		t.Fatalf("redirect: %d %v", n, err)
	}
	// Per-IP budget: a burst beyond 40 replies is dropped silently.
	req := packet{Type: typeRequest}
	rb := req.marshal()
	for range 60 {
		if _, err := client.WriteToUDPAddrPort(rb[:], set.Primary); err != nil {
			t.Fatal(err)
		}
	}
	answered := 0
	for {
		_ = client.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		if _, _, err := client.ReadFromUDPAddrPort(buf); err != nil {
			break
		}
		answered++
	}
	if answered < 30 || answered > 41 {
		t.Fatalf("rate limit: %d answered", answered)
	}
	_ = s
}
func itoa(v uint16) string {
	return netip.AddrPortFrom(netip.IPv4Unspecified(), v).String()[len("0.0.0.0:"):]
}

func TestDescriptorAndProfileStreams(t *testing.T) {
	newHost := func() *host.Host {
		key, err := wire.GenerateNodeKey()
		if err != nil {
			t.Fatal(err)
		}
		h, err := host.New(host.Config{Key: key, Listen: []string{"127.0.0.1:0"}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { h.Close() })
		return h
	}
	server, client := newHost(), newHost()
	s := loopbackServer(t, ServerSet{Listen: EndpointSet{Primary: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), freePort(t))}})
	if err := s.Register(server); err != nil {
		t.Fatal(err)
	}
	want := Profile{UDP: UDPOK, Mapping: MappingEndpointIndependent, Observers: 2}
	if err := ServeProfiles(server, func() (Profile, bool) { return want, true }); err != nil {
		t.Fatal(err)
	}
	if err := ServeProfiles(client, func() (Profile, bool) { return Profile{}, false }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr := server.Addrs()[0].String() + "/p2p/" + server.ID().String()
	ma, err := wire.ParseMultiaddr(addr, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Dial(ctx, ma); err != nil {
		t.Fatal(err)
	}
	d, err := Fetch(ctx, client, server.ID())
	if err != nil || len(d.Sets) != 1 || d.Sets[0] != s.Descriptor().Sets[0] {
		t.Fatalf("fetch: %+v %v", d, err)
	}
	got, ok, err := FetchProfile(ctx, client, server.ID())
	if err != nil || !ok || got != want {
		t.Fatalf("profile: %+v %t %v", got, ok, err)
	}
	if _, ok, err := FetchProfile(ctx, server, client.ID()); err != nil || ok {
		t.Fatalf("empty profile: %t %v", ok, err)
	}
}

func TestAllocationClassifier(t *testing.T) {
	mk := func(locals, mapped []uint16) []Sample {
		var out []Sample
		for i := range locals {
			out = append(out, Sample{netip.AddrPortFrom(netip.MustParseAddr("10.0.0.2"), locals[i]), netip.AddrPortFrom(netip.MustParseAddr("198.18.0.20"), mapped[i])})
		}
		return out
	}
	for _, tc := range []struct {
		locals, mapped []uint16
		want           Allocation
		delta          int16
		jitter         uint16
	}{
		{[]uint16{5000, 5100, 5200, 5300, 5400}, []uint16{5000, 5100, 5200, 5300, 5401}, AllocPreserving, 0, 0},
		{[]uint16{5000, 5100, 5200, 5300, 5400}, []uint16{40000, 40007, 40014, 40021, 40028}, AllocSequential, 7, 0},
		{[]uint16{5000, 5100, 5200, 5300, 5400}, []uint16{40000, 40002, 40003, 40009, 40011}, AllocSequential, 2, 4},
		{[]uint16{5000, 5100, 5200, 5300, 5400}, []uint16{65534, 65535, 0, 1, 2}, AllocSequential, 1, 0},
		{[]uint16{5000, 5100, 5200, 5300, 5400}, []uint16{12000, 51000, 23000, 33333, 18000}, AllocRandom, 0, 0},
	} {
		a, d, j := classifyAllocation(mk(tc.locals, tc.mapped), 256, 64)
		if a != tc.want || d != tc.delta || j != tc.jitter {
			t.Fatalf("%v: %s %d %d", tc.mapped, a, d, j)
		}
	}
}

func TestMonitor(t *testing.T) {
	s := loopbackServer(t, ServerSet{Listen: EndpointSet{Primary: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), freePort(t))}})
	reports := make(chan Report, 8)
	m, err := StartMonitor(context.Background(), MonitorConfig{
		Config:        Config{Timeout: 200 * time.Millisecond, Retransmit: 50 * time.Millisecond, LifetimeMin: 50 * time.Millisecond, LifetimeMax: 50 * time.Millisecond},
		Observers:     func(context.Context) ([]Observer, error) { return Observers("one", s.Descriptor()), nil },
		RetryInterval: 20 * time.Millisecond,
		OnChange:      func(r Report) { reports <- r },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	waitLifetime := func() Report {
		t.Helper()
		for {
			select {
			case r := <-reports:
				if r.Lifetime != nil {
					return r
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no report with lifetime")
			}
		}
	}
	if r := waitLifetime(); r.Profile.UDP != UDPOK || r.Profile.LifetimeBound != BoundAtLeast {
		t.Fatalf("first run: %s", r.Profile)
	}
	if err := m.Hook()(context.Background(), netmon.Event{}); err != nil {
		t.Fatal(err)
	}
	waitLifetime() // a network change re-measures the lifetime
	if p, ok := m.Profile(); !ok || p.UDP != UDPOK {
		t.Fatalf("profile %s %t", p, ok)
	}
}
