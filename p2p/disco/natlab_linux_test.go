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
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/magicsock"
	"github.com/whyrusleeping/nu/p2p/natlab"
	"github.com/whyrusleeping/nu/p2p/netmon"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// The lab uses a process-bridged Unix stream as the stable framed relay leg, not
// a fake UDP NAT and not a claim to test circuit reservation/reconnection. All
// direct probes and inner QUIC use real UDP, namespaces and nftables fixtures.
type labConfig struct {
	Key                    []byte
	Peer, Relay, Advertise string
}
type labCommand struct{ Op, Address string }
type labReply struct {
	Error    string
	Status   Status
	Progress int64
	Alive    bool
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
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 4000})
	check(t, err)
	// Reflexive candidates imply an existing observer mapping. Prime the fixture
	// before exchange, avoiding nft conntrack's cached pre-mapping DNAT miss.
	_, err = udp.WriteToUDP([]byte("fixture observer"), &net.UDPAddr{IP: net.ParseIP("198.18.0.1"), Port: 5000})
	check(t, err)
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
	d, err := New(ctx, Config{Host: h, Socket: m, Candidates: func(wire.PeerID) []Candidate { return []Candidate{candidate(advertised.Load().(netip.AddrPort))} }, Authorize: func(p wire.PeerID, c Candidate) bool {
		return p == peer && netip.MustParsePrefix("198.18.0.0/24").Contains(c.Addr.Addr()) && c.Addr.Port() == 4000
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
		case "open":
			ma, e := wire.ParseMultiaddr(fmt.Sprintf("/ip4/%s/udp/%d/quic-v1/p2p/%s", v.Addr(), v.Port(), peer), 1024)
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
			transferDone = make(chan error, 1)
			go func() {
				s, e := h.OpenStream(ctx, peer, "/disco-lab-echo/1")
				if e != nil {
					transferDone <- e
					return
				}
				defer s.Close()
				_ = s.SetDeadline(time.Now().Add(8 * time.Second))
				for i := 0; i < 256; i++ {
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
					time.Sleep(5 * time.Millisecond)
				}
				if conn == nil || s.Conn() != conn || conn.Context().Err() != nil {
					transferDone <- fmt.Errorf("inner connection replaced")
					return
				}
				transferDone <- nil
			}()
		case "result":
			err = <-transferDone
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
	for _, filter := range []natlab.Filtering{natlab.FilterEndpointIndependent, natlab.FilterAddressPortDependent} {
		t.Run(string(filter), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
				check(t, json.NewEncoder(in).Encode(labConfig{keys[i].Marshal(), keys[1-i].Public().PeerID().String(), path, fmt.Sprintf("198.18.0.%d:4000", 20+i)}))
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
			peers[0].call(t, labCommand{Op: "transfer"})
			eventually(t, func() bool { return peers[0].call(t, labCommand{Op: "status"}).Progress >= 128<<10 })
			before := peers[0].call(t, labCommand{Op: "status"}).Progress
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
			if !result.Alive || result.Progress != 4<<20 || result.Progress <= before {
				t.Fatalf("progress not preserved: %+v", result)
			}
			t.Logf("address change -> direct recovery %v; verified %d bytes, prior progress %d; same inner connection and stream remained open", recovered, result.Progress, before)
		})
	}
}
