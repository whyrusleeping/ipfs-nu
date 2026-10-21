//go:build natlab

package portmap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/p2p/natlab"
)

type labCommand struct {
	Op       string
	Protocol Protocol
}
type labReply struct {
	Mapping Mapping
	Error   string
}
type labRPC struct {
	enc *json.Encoder
	dec *json.Decoder
}

func (r *labRPC) call(cmd labCommand) (Mapping, error) {
	if err := r.enc.Encode(cmd); err != nil {
		return Mapping{}, err
	}
	var reply labReply
	if err := r.dec.Decode(&reply); err != nil {
		return Mapping{}, err
	}
	if reply.Error != "" {
		return Mapping{}, fmt.Errorf("helper: %s", reply.Error)
	}
	return reply.Mapping, nil
}
func startLabRPC(ns *natlab.Namespace, mode string) (*labRPC, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	in, out, err := ns.Start("env", "NU_PORTMAP_LAB_MODE="+mode, exe)
	if err != nil {
		return nil, err
	}
	rpc := &labRPC{enc: json.NewEncoder(in), dec: json.NewDecoder(out)}
	var ready labReply
	if err = rpc.dec.Decode(&ready); err != nil {
		return nil, err
	}
	if ready.Error != "" {
		return nil, fmt.Errorf("helper: %s", ready.Error)
	}
	return rpc, nil
}
func init() {
	mode := os.Getenv("NU_PORTMAP_LAB_MODE")
	if mode == "" {
		return
	}
	if os.Getenv("NU_NATLAB_ISOLATED") != "1" {
		fmt.Fprintln(os.Stderr, "portmap helper requires isolated lab")
		os.Exit(2)
	}
	if err := runLabHelper(mode); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
func nftMapping(local netip.AddrPort, external uint16, seconds uint32) error {
	rules := "flush chain ip nu_portmap mapping\n"
	if seconds != 0 {
		if local.Addr() != netip.MustParseAddr("10.0.0.2") || local.Port() != 4001 || external == 0 {
			return fmt.Errorf("invalid test mapping")
		}
		rules += fmt.Sprintf("add rule ip nu_portmap mapping iifname \"wan\" ip daddr 198.18.0.20 udp dport %d dnat to %s\n", external, local)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft: %w: %s", err, out)
	}
	return nil
}
func runLabHelper(mode string) error {
	dec, enc := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	var fake *fakeGateway
	var c *Client
	if mode == "router" {
		var err error
		fake, err = newFake("10.0.0.1", 5351, 1900, nftMapping)
		if err != nil {
			return err
		}
		defer fake.close()
	} else if mode != "client" {
		return fmt.Errorf("unknown helper mode")
	}
	if err := enc.Encode(labReply{}); err != nil {
		return err
	}
	for {
		var cmd labCommand
		if err := dec.Decode(&cmd); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		var m Mapping
		var err error
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if fake != nil {
			switch cmd.Op {
			case "reset":
				fake.epoch.Store(1000)
				err = nftMapping(netip.AddrPort{}, 0, 0)
			case "reboot":
				fake.epoch.Store(0)
				err = nftMapping(netip.AddrPort{}, 0, 0)
			default:
				err = fmt.Errorf("unknown router operation")
			}
		} else {
			switch cmd.Op {
			case "map":
				c, err = New(Config{Gateway: netip.MustParseAddr("10.0.0.1"), Local: netip.MustParseAddrPort("10.0.0.2:4001"), Protocols: []Protocol{cmd.Protocol}})
				if err == nil {
					m, err = c.Refresh(ctx)
				}
			case "check":
				if c == nil {
					err = fmt.Errorf("no client")
				} else {
					err = c.Check(ctx)
					m, _ = c.Candidate()
				}
			case "release":
				if c == nil {
					err = fmt.Errorf("no client")
				} else {
					err = c.Release(ctx)
				}
			default:
				err = fmt.Errorf("unknown client operation")
			}
		}
		cancel()
		reply := labReply{Mapping: m}
		if err != nil {
			reply.Error = err.Error()
		}
		if err = enc.Encode(reply); err != nil {
			return err
		}
	}
}
func TestNATLabPortMappings(t *testing.T) {
	if !natlab.Isolate(t) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	lab, err := natlab.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lab.Close()
	internet, err := lab.Namespace("internet")
	if err != nil {
		t.Fatal(err)
	}
	public, err := internet.Segment("public", "198.18.0.1/24")
	if err != nil {
		t.Fatal(err)
	}
	router, err := lab.Namespace("router")
	if err != nil {
		t.Fatal(err)
	}
	if err = public.Attach(router, "wan", "198.18.0.20/24", ""); err != nil {
		t.Fatal(err)
	}
	lan, err := router.Segment("lan", "10.0.0.1/24")
	if err != nil {
		t.Fatal(err)
	}
	client, err := lab.Namespace("client")
	if err != nil {
		t.Fatal(err)
	}
	if err = lan.Attach(client, "eth0", "10.0.0.2/24", "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	observer, err := lab.Namespace("observer")
	if err != nil {
		t.Fatal(err)
	}
	if err = public.Attach(observer, "eth0", "198.18.0.30/24", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = router.Run("sysctl", "-qw", "net.ipv4.ip_forward=1"); err != nil {
		t.Fatal(err)
	}
	if err = router.NFT(`table ip nu_portmap {
 chain mapping { type nat hook prerouting priority dstnat; policy accept; }
 chain forward { type filter hook forward priority filter; policy drop;
  ct state established,related accept
  iifname "lan" accept
  ct status dnat accept
 }
 chain outbound { type nat hook postrouting priority srcnat; policy accept; oifname "wan" masquerade; }
}`); err != nil {
		t.Fatal(err)
	}
	// The mapped application has never sent to the public observer. Every check
	// uses a new source socket, so old conntrack replies cannot fake an inbound map.
	if _, err = client.StartHost(natlab.HostConfig{Echo: []string{"10.0.0.2:4001"}}); err != nil {
		t.Fatal(err)
	}
	server, err := startLabRPC(router, "router")
	if err != nil {
		t.Fatal(err)
	}
	mapper, err := startLabRPC(client, "client")
	if err != nil {
		t.Fatal(err)
	}
	probe := func(target string, want bool) {
		t.Helper()
		node, err := observer.StartHost(natlab.HostConfig{ProbeListen: "198.18.0.30:0"})
		if err != nil {
			t.Fatal(err)
		}
		seen, err := node.Probe(target)
		if !want {
			if err == nil {
				t.Fatal("unmapped endpoint reachable", seen)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		addr, err := netip.ParseAddrPort(seen)
		if err != nil || addr.Addr() != netip.MustParseAddr("198.18.0.30") {
			t.Fatal("bad echoed endpoint", seen, err)
		}
	}
	for _, p := range []Protocol{PCP, PMP, UPnP} {
		start := time.Now()
		if _, err = server.call(labCommand{Op: "reset"}); err != nil {
			t.Fatal(err)
		}
		m, err := mapper.call(labCommand{Op: "map", Protocol: p})
		if err != nil {
			t.Fatal(err)
		}
		probe(m.External.String(), true)
		if _, err = server.call(labCommand{Op: "reboot"}); err != nil {
			t.Fatal(err)
		}
		m, err = mapper.call(labCommand{Op: "check"})
		if err != nil || !m.Reboot {
			t.Fatal("remap after reboot", m, err)
		}
		probe(m.External.String(), true)
		if _, err = mapper.call(labCommand{Op: "release"}); err != nil {
			t.Fatal(err)
		}
		probe(m.External.String(), false)
		t.Logf("protocol=%s mapping+epoch-zero-reboot+delete via real nft DNAT: %v", p, time.Since(start))
	}
}
