//go:build linux && natlab

package netcheck_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/natlab"
	"github.com/whyrusleeping/nu/p2p/netcheck"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/relay"
)

const helperEnv = "NU_NETCHECK_LAB_HELPER"

// Helpers are re-executions of this test binary inside a lab namespace; they
// speak JSON lines on stdin/stdout. Only meaningful under natlab isolation.
func init() {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	var err error
	switch mode {
	case "observer":
		err = observerHelper(os.Stdin, os.Stdout)
	case "client":
		err = clientHelper(os.Stdin, os.Stdout)
	default:
		err = fmt.Errorf("unknown helper %q", mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "netcheck helper:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

type observerSpec struct {
	Listen string
	Set    netcheck.EndpointSet
}
type started struct{ Address, Error string }

func observerHelper(in io.Reader, out io.Writer) error {
	var spec observerSpec
	dec := json.NewDecoder(in)
	if err := dec.Decode(&spec); err != nil {
		return err
	}
	key, err := wire.GenerateNodeKey()
	if err != nil {
		return err
	}
	s, err := relay.New(relay.Config{Host: host.Config{Key: key, Listen: []string{spec.Listen}},
		Netcheck: &netcheck.ServerConfig{Sets: []netcheck.ServerSet{{Listen: spec.Set}}}})
	if err != nil {
		return err
	}
	defer s.Close()
	if err := json.NewEncoder(out).Encode(started{Address: s.Host.Addrs()[0].String() + "/p2p/" + s.Host.ID().String()}); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, in) // until the lab closes our stdin
	return nil
}

type clientSpec struct {
	Listen    string   // QUIC host socket
	Relays    []string // observer multiaddrs
	ProbeIP   string
	Ports     []uint16 // probe sockets in opening order
	LifeMin   time.Duration
	LifeMax   time.Duration
	SkipLife  bool
	WaitForGo bool
}
type clientResult struct {
	Report netcheck.Report
	Error  string
}

func clientHelper(in io.Reader, out io.Writer) error {
	dec, enc := json.NewDecoder(in), json.NewEncoder(out)
	var spec clientSpec
	if err := dec.Decode(&spec); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	key, err := wire.GenerateNodeKey()
	if err != nil {
		return err
	}
	h, err := host.New(host.Config{Key: key, Listen: []string{spec.Listen}})
	if err != nil {
		return err
	}
	defer h.Close()
	var observers []netcheck.Observer
	for _, r := range spec.Relays {
		a, err := wire.ParseMultiaddr(r, 1024)
		if err != nil {
			return err
		}
		conn, err := h.Dial(ctx, a)
		if err != nil {
			return err
		}
		d, err := netcheck.Fetch(ctx, h, conn.PeerID())
		if err != nil {
			return err
		}
		observers = append(observers, netcheck.Observers(conn.PeerID().String(), d)...)
	}
	if spec.WaitForGo {
		if err := enc.Encode(started{Address: "fetched"}); err != nil {
			return err
		}
		var goSignal string
		if err := dec.Decode(&goSignal); err != nil {
			return err
		}
	}
	next := 0
	cfg := netcheck.Config{
		Timeout: 600 * time.Millisecond, Retransmit: 150 * time.Millisecond,
		LifetimeMin: spec.LifeMin, LifetimeMax: spec.LifeMax,
		ListenPacket: func(network string) (net.PacketConn, error) {
			if next >= len(spec.Ports) {
				return nil, fmt.Errorf("lab ran out of fixture probe ports")
			}
			next++
			return net.ListenPacket(network, net.JoinHostPort(spec.ProbeIP, strconv.Itoa(int(spec.Ports[next-1]))))
		},
	}
	var r netcheck.Report
	if spec.SkipLife {
		r, err = netcheck.Probe(ctx, cfg, observers)
	} else {
		r, err = netcheck.Run(ctx, cfg, observers)
	}
	res := clientResult{Report: r}
	if err != nil {
		res.Error = err.Error()
	}
	return enc.Encode(res)
}

type proc struct {
	in  io.WriteCloser
	enc *json.Encoder
	dec *json.Decoder
}

func startHelper(t *testing.T, ns *natlab.Namespace, mode string, spec any) *proc {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, out, err := ns.Start("env", helperEnv+"="+mode, exe)
	if err != nil {
		t.Fatal(err)
	}
	p := &proc{in: in, enc: json.NewEncoder(in), dec: json.NewDecoder(bufio.NewReader(out))}
	if err := p.enc.Encode(spec); err != nil {
		t.Fatal(err)
	}
	return p
}
func (p *proc) read(t *testing.T, v any) {
	t.Helper()
	if err := p.dec.Decode(v); err != nil {
		t.Fatal(err)
	}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

type scenario struct {
	name     string
	nat      natlab.NATConfig
	outer    *natlab.NATConfig // double NAT: carrier-grade outer router
	altIP    bool              // observer 1 has an alternate IP
	blockUDP bool
	// expectations
	mapping   netcheck.Mapping
	filtering netcheck.Filtering
	alloc     netcheck.Allocation
	hairpin   netcheck.Hairpin
	lifetime  time.Duration
}

const (
	clientIP   = "10.0.0.2"
	firstPort  = 5000
	probePorts = 16 // 6 probe + 1 lifetime control + 5 coarse + 3 fine rungs, +1 spare
)

// TestNetcheckNATLab is 0018's acceptance: each configured fixture behaviour
// (mapping, allocation, filtering, hairpin, lifetime ±20%) is classified from
// real packets through the netns NAT, on single and double IPv4 NAT, plus UDP
// blocking. Scenarios run in parallel, each in its own lab.
func TestNetcheckNATLab(t *testing.T) {
	if !natlab.IsolateTimeout(t, 100*time.Second) {
		return
	}
	nat := func(m natlab.Mapping, f natlab.Filtering, a natlab.Allocation, life time.Duration, hairpin bool) natlab.NATConfig {
		return natlab.NATConfig{Mapping: m, Filtering: f, Allocation: a, Lifetime: life, Hairpin: hairpin, ExpireFlows: true}
	}
	outer := nat(natlab.AddressDependent, natlab.FilterAddressDependent, natlab.Sequential, 6*time.Second, false)
	for _, sc := range []scenario{
		{name: "eim-eif-preserve-hairpin", nat: nat(natlab.EndpointIndependent, natlab.FilterEndpointIndependent, natlab.Preserve, 5*time.Second, true), altIP: true,
			mapping: netcheck.MappingEndpointIndependent, filtering: netcheck.FilterEndpointIndependent, alloc: netcheck.AllocPreserving, hairpin: netcheck.HairpinYes, lifetime: 5 * time.Second},
		{name: "eim-eif-sequential-crossobserver", nat: nat(natlab.EndpointIndependent, natlab.FilterEndpointIndependent, natlab.Sequential, 7*time.Second, false),
			mapping: netcheck.MappingEndpointIndependent, filtering: netcheck.FilterEndpointIndependent, alloc: netcheck.AllocSequential, hairpin: netcheck.HairpinNo, lifetime: 7 * time.Second},
		{name: "adm-adf-sequential", nat: nat(natlab.AddressDependent, natlab.FilterAddressDependent, natlab.Sequential, 5*time.Second, false), altIP: true,
			mapping: netcheck.MappingAddressDependent, filtering: netcheck.FilterAddressDependent, alloc: netcheck.AllocSequential, lifetime: 5 * time.Second},
		{name: "apdm-apdf-random", nat: nat(natlab.AddressPortDependent, natlab.FilterAddressPortDependent, natlab.Random, 9*time.Second, false), altIP: true,
			mapping: netcheck.MappingAddressPortDependent, filtering: netcheck.FilterAddressPortDependent, alloc: netcheck.AllocRandom, lifetime: 9 * time.Second},
		{name: "eim-apdf-random", nat: nat(natlab.EndpointIndependent, natlab.FilterAddressPortDependent, natlab.Random, 4*time.Second, false), altIP: true,
			mapping: netcheck.MappingEndpointIndependent, filtering: netcheck.FilterAddressPortDependent, alloc: netcheck.AllocRandom, lifetime: 4 * time.Second},
		{name: "apdm-eif-preserve", nat: nat(natlab.AddressPortDependent, natlab.FilterEndpointIndependent, natlab.Preserve, 6*time.Second, false), altIP: true,
			mapping: netcheck.MappingAddressPortDependent, filtering: netcheck.FilterEndpointIndependent, alloc: netcheck.AllocPreserving, hairpin: netcheck.HairpinNo, lifetime: 6 * time.Second},
		{name: "double-nat-eim-inside-adm-cgnat", nat: nat(natlab.EndpointIndependent, natlab.FilterEndpointIndependent, natlab.Preserve, time.Minute, false), outer: &outer,
			mapping: netcheck.MappingAddressDependent, filtering: netcheck.FilterAddressDependent, alloc: netcheck.AllocSequential, lifetime: 6 * time.Second},
		{name: "udp-blocked", nat: nat(natlab.EndpointIndependent, natlab.FilterEndpointIndependent, natlab.Preserve, time.Minute, false), blockUDP: true},
	} {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			runScenario(t, sc)
		})
	}
}

func runScenario(t *testing.T, sc scenario) {
	begin := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	lab, err := natlab.New(ctx)
	check(t, err)
	defer lab.Close()
	internet, err := lab.Namespace("internet")
	check(t, err)
	public, err := internet.Segment("br0", "198.18.0.1/24")
	check(t, err)
	obs1, err := lab.Namespace("observer1")
	check(t, err)
	check(t, public.Attach(obs1, "eth0", "198.18.0.10/24", ""))
	check(t, obs1.Address("eth0", "", "198.18.0.11/24"))
	obs2, err := lab.Namespace("observer2")
	check(t, err)
	check(t, public.Attach(obs2, "eth0", "198.18.0.30/24", ""))
	set1 := netcheck.EndpointSet{Primary: ap("198.18.0.10:3478"), AltPort: ap("198.18.0.10:3479")}
	if sc.altIP {
		set1.AltIP = ap("198.18.0.11:3478")
	}
	set2 := netcheck.EndpointSet{Primary: ap("198.18.0.30:3478"), AltPort: ap("198.18.0.30:3479")}
	destinations := []netip.AddrPort{set1.Primary, set1.AltPort, ap("198.18.0.11:3478"), set2.Primary, set2.AltPort, ap("198.18.0.10:4000"), ap("198.18.0.30:4000")}

	// Router chain: [outer CGNAT] -> home router -> client.
	external := netip.MustParseAddr("198.18.0.20")
	homeWAN, homeGW := "198.18.0.20/24", ""
	var carrier *natlab.Segment
	var cgnat *natlab.Namespace
	if sc.outer != nil {
		cgnat, err = lab.Namespace("cgnat")
		check(t, err)
		check(t, public.Attach(cgnat, "wan", "198.18.0.20/24", ""))
		carrier, err = cgnat.Segment("cgn0", "100.64.0.1/24")
		check(t, err)
		homeWAN, homeGW = "100.64.0.2/24", "100.64.0.1"
	}
	home, err := lab.Namespace("home")
	check(t, err)
	if carrier != nil {
		check(t, carrier.Attach(home, "wan", homeWAN, homeGW))
	} else {
		check(t, public.Attach(home, "wan", homeWAN, homeGW))
	}
	lan, err := home.Segment("lan", "10.0.0.1/24")
	check(t, err)
	client, err := lab.Namespace("client")
	check(t, err)
	check(t, lan.Attach(client, "eth0", clientIP+"/24", "10.0.0.1"))

	var inside []netip.AddrPort
	var ports []uint16
	for i := range probePorts {
		p := uint16(firstPort + 100*i)
		ports = append(ports, p)
		inside = append(inside, netip.AddrPortFrom(netip.MustParseAddr(clientIP), p))
	}
	inside = append(inside, ap(clientIP+":4000")) // QUIC host last: keeps allocation order
	cfg := sc.nat
	cfg.WAN, cfg.LAN, cfg.Inside, cfg.Destinations = "wan", "lan", inside, destinations
	cfg.ExternalIP = netip.MustParseAddr(homeWAN[:len(homeWAN)-3])
	homeNAT, err := home.NAT(cfg)
	check(t, err)
	natForAlloc := homeNAT
	if sc.outer != nil {
		oc := *sc.outer
		oc.WAN, oc.LAN, oc.ExternalIP, oc.Destinations = "wan", "cgn0", external, destinations
		seen := map[netip.AddrPort]bool{}
		for _, b := range homeNAT.Bindings {
			if !seen[b.Outside] {
				seen[b.Outside] = true
				oc.Inside = append(oc.Inside, b.Outside)
			}
		}
		natForAlloc, err = cgnat.NAT(oc)
		check(t, err)
	}

	var o1, o2 started
	p1 := startHelper(t, obs1, "observer", observerSpec{Listen: "198.18.0.10:4000", Set: set1})
	p2 := startHelper(t, obs2, "observer", observerSpec{Listen: "198.18.0.30:4000", Set: set2})
	p1.read(t, &o1)
	p2.read(t, &o2)
	lifeMax := 16 * time.Second
	cp := startHelper(t, client, "client", clientSpec{Listen: clientIP + ":4000", Relays: []string{o1.Address, o2.Address}, ProbeIP: clientIP, Ports: ports,
		LifeMin: time.Second, LifeMax: lifeMax, SkipLife: sc.blockUDP, WaitForGo: sc.blockUDP})
	if sc.blockUDP {
		// The descriptor arrived over QUIC; now block all UDP, as if the control
		// connection were on the HTTPS relay transport.
		var fetched started
		cp.read(t, &fetched)
		check(t, home.NFT("delete table ip nu_natlab"))
		cfg.BlockUDP = true
		_, err = home.NAT(cfg)
		check(t, err)
		check(t, cp.enc.Encode("go"))
	}
	var res clientResult
	cp.read(t, &res)
	b, _ := json.MarshalIndent(res.Report, "", "  ")
	t.Logf("%s: %s\n%s", sc.name, res.Report.Profile, b)
	if res.Error != "" {
		t.Fatal(res.Error)
	}
	p := res.Report.Profile
	if sc.blockUDP {
		if p.UDP != netcheck.UDPBlocked {
			t.Fatalf("want udp=blocked, got %s", p)
		}
		emit(t, sc.name, p, begin)
		return
	}
	if p.UDP != netcheck.UDPOK || p.Mapping != sc.mapping || p.Filtering != sc.filtering || p.Allocation != sc.alloc || p.Hairpin != sc.hairpin || p.VaryingIP {
		t.Fatalf("misclassified: got %s", p)
	}
	// >=3 observer endpoints on >=2 IPs (3 IPs when observer 1 has an alternate IP).
	if len(res.Report.IPv4.Observations) < 3 || int(p.Observers) < 2 || (sc.altIP && p.Observers < 3) {
		t.Fatalf("observer coverage: %d endpoints, %d IPs", len(res.Report.IPv4.Observations), p.Observers)
	}
	if sc.alloc == netcheck.AllocSequential {
		// The fixture reserves ports in Inside x Destinations order: the expected
		// delta is the gap between the first two allocation sockets toward O1.
		port := func(local uint16) uint16 {
			want := netip.AddrPortFrom(netip.MustParseAddr(clientIP), local)
			if sc.outer != nil {
				for _, b := range homeNAT.Bindings {
					if b.Inside == want && (!b.Remote.IsValid() || b.Remote.Addr() == set1.Primary.Addr()) {
						want = b.Outside
						break
					}
				}
			}
			for _, b := range natForAlloc.Bindings {
				if b.Inside == want && (!b.Remote.IsValid() || b.Remote.Addr() == set1.Primary.Addr()) && (b.Remote.Port() == 0 || b.Remote.Port() == set1.Primary.Port()) {
					return b.Outside.Port()
				}
			}
			t.Fatalf("no binding for %s", want)
			return 0
		}
		want := int16(port(ports[2]) - port(ports[1]))
		if p.Delta != want || p.Jitter != 0 {
			t.Fatalf("sequential delta: got %+d jitter %d, want %+d", p.Delta, p.Jitter, want)
		}
	}
	l := res.Report.Lifetime
	if l == nil || l.Bound != netcheck.BoundExact {
		t.Fatalf("lifetime not bracketed: %+v", l)
	}
	if ratio := float64(l.Estimate) / float64(sc.lifetime); math.Abs(ratio-1) > 0.2 || math.Abs(float64(p.Lifetime)/sc.lifetime.Seconds()-1) > 0.2 {
		t.Fatalf("lifetime %v (encoded %ds) not within 20%% of %v", l.Estimate, p.Lifetime, sc.lifetime)
	}
	emit(t, sc.name, p, begin)
}

func emit(t *testing.T, name string, p netcheck.Profile, begin time.Time) {
	t.Helper()
	b, err := json.Marshal(natlab.Outcome{Scenario: "netcheck/" + name, From: "client", To: "observers", Outcome: p.String(), Elapsed: time.Since(begin)})
	check(t, err)
	t.Log(string(b))
	if path := os.Getenv("NU_NATLAB_RESULTS"); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		check(t, err)
		_, err = f.Write(append(b, '\n'))
		check(t, err)
		check(t, f.Close())
	}
}
