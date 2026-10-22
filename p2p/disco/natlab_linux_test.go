//go:build linux && natlab

package disco

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/nutest/slow"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/magicsock"
	"github.com/whyrusleeping/nu/p2p/natlab"
	"github.com/whyrusleeping/nu/p2p/netcheck"
	"github.com/whyrusleeping/nu/p2p/netmon"
	"github.com/whyrusleeping/nu/p2p/portmap"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// The lab uses a process-bridged Unix stream as the stable framed relay leg, not
// a fake UDP NAT and not a claim to test circuit reservation/reconnection. All
// direct probes and inner QUIC use real UDP, namespaces and nftables fixtures.
type labConfig struct {
	Key                              []byte
	Peer, Relay, Advertise           string
	Listen, Scope, Gateway, Internal string
	Hard                             bool
	Observers                        []netcheck.Observer
	Noise                            time.Duration
}
type labCommand struct {
	Op, Address string
	Bytes       int64
}
type labReply struct {
	Error    string
	Status   Status
	Progress int64
	Alive    bool
	Mapping  portmap.Mapping
	Report   netcheck.Report
}
type labPeer struct {
	in  io.WriteCloser
	dec *json.Decoder
}

func (p *labPeer) call(t *testing.T, c labCommand) labReply {
	t.Helper()
	check(t, json.NewEncoder(p.in).Encode(c))
	var r labReply
	check(t, p.dec.Decode(&r))
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	return r
}

type noisePacket struct {
	net.PacketConn
	observer netip.AddrPort
	once     sync.Once
}

func (p *noisePacket) WriteTo(b []byte, a net.Addr) (int, error) {
	p.once.Do(func() {
		u, err := net.ListenUDP("udp4", &net.UDPAddr{})
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			_, _ = netcheck.Observe(ctx, u, p.observer)
			cancel()
			u.Close()
		}
	})
	return p.PacketConn.WriteTo(b, a)
}

func TestDiscoLabHelper(t *testing.T) {
	if os.Getenv("NU_DISCO_HELPER") != "1" {
		return
	}
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	var cfg labConfig
	check(t, dec.Decode(&cfg))
	k, err := wire.ParseNodeKey(cfg.Key, 1024)
	check(t, err)
	peer, err := wire.ParsePeerID(cfg.Peer)
	check(t, err)
	network := "udp4"
	if cfg.Listen == "" {
		cfg.Listen = "0.0.0.0:4000"
	}
	if strings.Contains(cfg.Listen, "[") {
		network = "udp6"
	}
	bind, err := net.ResolveUDPAddr(network, cfg.Listen)
	check(t, err)
	udp, err := net.ListenUDP(network, bind)
	check(t, err)
	var report netcheck.Report
	if cfg.Hard {
		noiseDone := make(chan struct{})
		defer close(noiseDone)
		if cfg.Noise > 0 {
			go func() {
				tick := time.NewTicker(cfg.Noise)
				defer tick.Stop()
				for {
					select {
					case <-noiseDone:
						return
					case <-tick.C:
						u, e := net.ListenUDP("udp4", &net.UDPAddr{})
						if e == nil {
							_, _ = u.WriteToUDPAddrPort([]byte("background allocation"), cfg.Observers[0].Primary)
							u.Close()
						}
					}
				}
			}()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		probeCfg := netcheck.Config{Timeout: 100 * time.Millisecond, Retransmit: 40 * time.Millisecond}
		if cfg.Noise > 0 {
			opens := 0
			probeCfg.ListenPacket = func(network string) (net.PacketConn, error) {
				opens++
				pc, err := net.ListenPacket(network, ":0")
				if err == nil && opens%2 == 0 {
					return &noisePacket{PacketConn: pc, observer: cfg.Observers[0].Primary}, nil
				}
				return pc, err
			}
		}
		report, err = netcheck.Probe(ctx, probeCfg, cfg.Observers)
		check(t, err)
		observed, e := netcheck.Observe(ctx, udp, cfg.Observers[0].Primary)
		check(t, e)
		cfg.Advertise = observed.String()
	}
	// Reflexive candidates imply an existing observer mapping. Prime only the
	// IPv4 EIM fixtures, avoiding conntrack's cached pre-mapping DNAT miss.
	if network == "udp4" {
		observer := netip.MustParseAddrPort(cfg.Advertise)
		ip := observer.Addr().As4()
		ip[3] = 1
		_, err = udp.WriteToUDP([]byte("fixture observer"), net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.AddrFrom4(ip), 5000)))
		check(t, err)
	}
	m, err := magicsock.New(magicsock.Config{UDP: udp})
	check(t, err)
	defer m.Close()
	v, err := m.AddPeer(peer)
	check(t, err)
	relay, err := net.Dial("unix", cfg.Relay)
	check(t, err)
	_, err = m.AttachRelay(peer, relay)
	check(t, err)
	h, err := host.New(host.Config{Key: k, PacketConns: []net.PacketConn{m}})
	check(t, err)
	defer h.Close()
	var advertised atomic.Value
	advertised.Store(netip.MustParseAddrPort(cfg.Advertise))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if cfg.Scope == "" {
		cfg.Scope = "198.18.0.0/24"
	}
	scope := netip.MustParsePrefix(cfg.Scope)
	var lease atomic.Value
	lease.Store(portmap.Mapping{})
	var hard *HardConfig
	if cfg.Hard {
		hard = &HardConfig{Profile: func() (netcheck.Report, bool) { return report, true }, Authorize: func(p wire.PeerID, ip netip.Addr) bool { return p == peer && scope.Contains(ip) }}
	}
	d, err := New(ctx, Config{Host: h, Socket: m, Hard: hard, Candidates: func(wire.PeerID) []Candidate {
		if m := lease.Load().(portmap.Mapping); m.External.IsValid() {
			c, ok := FromMapping(m, netip.AddrPort{}, time.Now())
			if !ok {
				return nil
			}
			return []Candidate{c}
		}
		c := candidate(advertised.Load().(netip.AddrPort))
		if cfg.Hard {
			c.Kind = Reflexive
		}
		if c.Addr.Addr().Is6() {
			c.Kind = IPv6
		}
		return []Candidate{c}
	}, Authorize: func(p wire.PeerID, c Candidate) bool {
		return p == peer && scope.Contains(c.Addr.Addr()) && c.Addr.Port() >= 1024
	}, Heartbeat: 100 * time.Millisecond, ProbeTimeout: 150 * time.Millisecond})
	check(t, err)
	defer d.Close()
	check(t, h.Handle("/disco-lab-echo/1", 2, func(s *host.Stream) { _, _ = io.Copy(s, s) }))
	var conn *host.Conn
	var progress atomic.Int64
	var transferDone chan error
	check(t, enc.Encode(labReply{Alive: true}))
	for {
		var command labCommand
		if dec.Decode(&command) != nil {
			return
		}
		r := labReply{}
		var err error
		switch command.Op {
		case "map":
			mapper, e := portmap.New(portmap.Config{Gateway: netip.MustParseAddr(cfg.Gateway), Local: netip.MustParseAddrPort(cfg.Internal), Protocols: []portmap.Protocol{portmap.UPnP}})
			err = e
			if err == nil {
				r.Mapping, err = mapper.Refresh(ctx)
				if err == nil {
					lease.Store(r.Mapping)
					advertised.Store(r.Mapping.External)
					d.Update()
				}
			}
		case "open":
			family := "ip4"
			if v.Addr().Is6() {
				family = "ip6"
			}
			ma, e := wire.ParseMultiaddr(fmt.Sprintf("/%s/%s/udp/%d/quic-v1/p2p/%s", family, v.Addr(), v.Port(), peer), 1024)
			err = e
			if err == nil {
				conn, err = h.Dial(ctx, ma)
			}
			if err == nil {
				err = d.Open(ctx, conn)
			}
		case "restart":
			if command.Address != "" {
				advertised.Store(netip.MustParseAddrPort(command.Address))
			}
			pipeline, e := netmon.NewPipeline(time.Second)
			err = e
			if err == nil {
				_, err = pipeline.Register(netmon.RestartProbing, d.RestartProbing)
			}
			if err == nil {
				events := make(chan netmon.Event, 1)
				events <- netmon.Event{}
				close(events)
				err = pipeline.Run(ctx, events)
			}
		case "transfer":
			total := command.Bytes
			if total == 0 {
				total = 4 << 20
			}
			pace := time.Duration(0)
			if total <= 4<<20 {
				pace = 5 * time.Millisecond
			}
			transferDone = make(chan error, 1)
			go func() {
				s, e := h.OpenStream(ctx, peer, "/disco-lab-echo/1")
				if e != nil {
					transferDone <- e
					return
				}
				defer s.Close()
				_ = s.SetDeadline(time.Now().Add(3 * time.Minute))
				for i := int64(0); i < total/(16<<10); i++ {
					data := bytes.Repeat([]byte{byte(i)}, 16<<10)
					if _, e = s.Write(data); e != nil {
						transferDone <- e
						return
					}
					got := make([]byte, len(data))
					if _, e = io.ReadFull(s, got); e != nil {
						transferDone <- e
						return
					}
					if !bytes.Equal(data, got) {
						transferDone <- fmt.Errorf("chunk %d corrupt", i)
						return
					}
					progress.Add(int64(len(data)))
					if pace > 0 {
						time.Sleep(pace)
					}
				}
				if conn == nil || s.Conn() != conn || conn.Context().Err() != nil {
					transferDone <- fmt.Errorf("inner connection replaced")
					return
				}
				transferDone <- nil
			}()
		case "result":
			err = <-transferDone
		case "profile":
			r.Report = report
		case "status":
		default:
			err = fmt.Errorf("unknown lab op")
		}
		if err != nil {
			r.Error = err.Error()
		}
		r.Status, _ = d.Status(peer)
		r.Progress = progress.Load()
		r.Alive = conn == nil || conn.Context().Err() == nil
		check(t, enc.Encode(r))
	}
}
func TestNATLabDisco(t *testing.T) {
	if !natlab.Isolate(t) {
		return
	}
	testNATDisco(t, false)
}
func TestNATLabDiscoGiB(t *testing.T) {
	slow.Skip(t)
	if !natlab.IsolateTimeout(t, 4*time.Minute) {
		return
	}
	testNATDisco(t, true)
}
func testNATDisco(t *testing.T, bulk bool) {
	filters := []natlab.Filtering{natlab.FilterEndpointIndependent, natlab.FilterAddressPortDependent}
	if bulk {
		filters = filters[1:]
	}
	for _, filter := range filters {
		t.Run(string(filter), func(t *testing.T) {
			timeout := 15 * time.Second
			if bulk {
				timeout = 3 * time.Minute
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			l, err := natlab.New(ctx)
			check(t, err)
			defer l.Close()
			internet, err := l.Namespace("internet")
			check(t, err)
			wan, err := internet.Segment("br0", "198.18.0.1/24")
			check(t, err)
			var clients, routers [2]*natlab.Namespace
			for i := range clients {
				routers[i], err = l.Namespace(fmt.Sprintf("router%d", i))
				check(t, err)
				clients[i], err = l.Namespace(fmt.Sprintf("peer%d", i))
				check(t, err)
				check(t, wan.Attach(routers[i], "wan", fmt.Sprintf("198.18.0.%d/24", 20+i), ""))
				lan, e := routers[i].Segment("lan", fmt.Sprintf("10.%d.0.1/24", i))
				check(t, e)
				check(t, lan.Attach(clients[i], "eth0", fmt.Sprintf("10.%d.0.2/24", i), fmt.Sprintf("10.%d.0.1", i)))
				filtering := natlab.FilterEndpointIndependent
				if i == 1 {
					filtering = filter
				}
				_, err = routers[i].NAT(natlab.NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr(fmt.Sprintf("198.18.0.%d", 20+i)), Inside: []netip.AddrPort{netip.MustParseAddrPort(fmt.Sprintf("10.%d.0.2:4000", i))}, Destinations: []netip.AddrPort{netip.MustParseAddrPort(fmt.Sprintf("198.18.0.%d:4000", 21-i))}, Mapping: natlab.EndpointIndependent, Filtering: filtering})
				check(t, err)
			}
			// Both helpers share a filesystem but have isolated networks. The Unix relay
			// keeps the framing leg stable across the intentional physical address change.
			path := filepath.Join(t.TempDir(), "relay")
			listener, err := net.Listen("unix", path)
			check(t, err)
			defer listener.Close()
			ka, kb := key(t), key(t)
			keys := []*wire.NodeKey{ka, kb}
			var peers [2]*labPeer
			for i := range peers {
				in, out, e := clients[i].Start("env", "NU_DISCO_HELPER=1", os.Args[0], "-test.run=^TestDiscoLabHelper$")
				check(t, e)
				peers[i] = &labPeer{in, json.NewDecoder(out)}
				check(t, json.NewEncoder(in).Encode(labConfig{Key: keys[i].Marshal(), Peer: keys[1-i].Public().PeerID().String(), Relay: path, Advertise: fmt.Sprintf("198.18.0.%d:4000", 20+i)}))
			}
			left, err := listener.Accept()
			check(t, err)
			defer left.Close()
			right, err := listener.Accept()
			check(t, err)
			defer right.Close()
			go func() { _, _ = io.Copy(left, right) }()
			go func() { _, _ = io.Copy(right, left) }()
			for _, p := range peers {
				var r labReply
				check(t, p.dec.Decode(&r))
				if !r.Alive {
					t.Fatal("helper not ready")
				}
			}
			started := time.Now()
			peers[0].call(t, labCommand{Op: "open"})
			eventually(t, func() bool {
				a, b := peers[0].call(t, labCommand{Op: "status"}), peers[1].call(t, labCommand{Op: "status"})
				return a.Status.Selected.IsValid() && b.Status.Selected.IsValid()
			})
			t.Logf("EIM/EIF <-> EIM/%s: both direct in %v", filter, time.Since(started))
			total := int64(4 << 20)
			if bulk {
				total = 1 << 30
			}
			transferStart := time.Now()
			peers[0].call(t, labCommand{Op: "transfer", Bytes: total})
			eventually(t, func() bool { return peers[0].call(t, labCommand{Op: "status"}).Progress >= 128<<10 })
			before := peers[0].call(t, labCommand{Op: "status"}).Progress
			if bulk {
				check(t, clients[0].Link("eth0", false))
				peers[0].call(t, labCommand{Op: "restart"})
				peers[1].call(t, labCommand{Op: "restart"})
				time.Sleep(200 * time.Millisecond)
				check(t, clients[0].Link("eth0", true))
				_, err = clients[0].Run("ip", "route", "replace", "default", "via", "10.0.0.1")
				check(t, err)
				peers[0].call(t, labCommand{Op: "restart"})
				peers[1].call(t, labCommand{Op: "restart"})
				eventually(t, func() bool {
					a, b := peers[0].call(t, labCommand{Op: "status"}), peers[1].call(t, labCommand{Op: "status"})
					return a.Status.Selected.IsValid() && b.Status.Selected.IsValid()
				})
				after := peers[0].call(t, labCommand{Op: "status"}).Progress
				if after <= before || after >= total {
					t.Fatalf("flap progress not preserved during transfer: before=%d after=%d", before, after)
				}
				t.Logf("standalone 200ms interface flap: progress %d -> %d; same stream still active", before, after)
			}
			check(t, clients[0].Link("eth0", false))
			peers[0].call(t, labCommand{Op: "restart"})
			peers[1].call(t, labCommand{Op: "restart"})
			// Move from one inside/outside address pair to another (hotspot-like) while
			// preserving the wildcard socket, relay leg, inner QUIC and application stream.
			check(t, clients[0].Address("eth0", "10.0.0.2/24", "10.0.0.3/24"))
			check(t, routers[0].Address("wan", "", "198.18.0.22/24"))
			_, err = routers[0].NAT(natlab.NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr("198.18.0.22"), Inside: []netip.AddrPort{netip.MustParseAddrPort("10.0.0.3:4000")}, Destinations: []netip.AddrPort{netip.MustParseAddrPort("198.18.0.21:4000")}, Mapping: natlab.EndpointIndependent, Filtering: natlab.FilterEndpointIndependent})
			check(t, err)
			check(t, clients[0].Link("eth0", true))
			_, err = clients[0].Run("ip", "route", "replace", "default", "via", "10.0.0.1")
			check(t, err)
			recovery := time.Now()
			peers[0].call(t, labCommand{Op: "restart", Address: "198.18.0.22:4000"})
			peers[1].call(t, labCommand{Op: "restart"})
			eventually(t, func() bool {
				return peers[1].call(t, labCommand{Op: "status"}).Status.Selected == netip.MustParseAddrPort("198.18.0.22:4000") && peers[0].call(t, labCommand{Op: "status"}).Status.Selected.IsValid()
			})
			recovered := time.Since(recovery)
			result := peers[0].call(t, labCommand{Op: "result"})
			if !result.Alive || result.Progress != total || result.Progress <= before {
				t.Fatalf("progress not preserved: %+v", result)
			}
			t.Logf("address change -> direct recovery %v; verified %d bytes in %v, prior progress %d; same inner connection and stream remained open", recovered, result.Progress, time.Since(transferStart), before)
		})
	}
}
