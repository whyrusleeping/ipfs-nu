//go:build linux && natlab

package swarm_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/link"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/natlab"
	"github.com/whyrusleeping/nu/p2p/wire"
)

type daemonLabProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
	log  string
}

func labCheck(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func labStart(t *testing.T, ns *natlab.Namespace, bin, dir string, args ...string) *daemonLabProcess {
	t.Helper()
	p := &daemonLabProcess{cmd: ns.Command(bin, append([]string{"--nu-path", dir, "--json"}, args...)...), done: make(chan struct{}), log: filepath.Join(t.TempDir(), "process.log")}
	out, err := os.Create(p.log)
	labCheck(t, err)
	p.cmd.Stdout = out
	p.cmd.Stderr = out
	labCheck(t, p.cmd.Start())
	go func() { p.err = p.cmd.Wait(); out.Close(); close(p.done) }()
	t.Cleanup(func() {
		p.stop()
		if t.Failed() {
			b, _ := os.ReadFile(p.log)
			t.Logf("%v: %s", args, b)
		}
	})
	return p
}
func (p *daemonLabProcess) stop() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}
func labCommand(ns *natlab.Namespace, bin, dir string, args ...string) ([]byte, error) {
	return ns.Command(bin, append([]string{"--nu-path", dir, "--json"}, args...)...).CombinedOutput()
}
func labRun(t *testing.T, ns *natlab.Namespace, bin, dir string, args ...string) []byte {
	t.Helper()
	out, err := labCommand(ns, bin, dir, args...)
	if err != nil {
		t.Fatalf("nu %v: %v: %s", args, err, out)
	}
	return out
}
func labConfig(t *testing.T, dir, key, value string) {
	t.Helper()
	r, _, err := repo.Open(dir)
	labCheck(t, err)
	labCheck(t, r.ConfigSet(key, value))
}
func labArray(v ...string) string { b, _ := json.Marshal(v); return string(b) }
func labStatus(ns *natlab.Namespace, bin, dir string) *apiv0.NetworkStatus {
	// Before the daemon publishes its API file, CLI status can fall back to an
	// in-process repo lock and race daemon startup. Wait for the real API first.
	if _, err := os.Stat(filepath.Join(dir, repo.APIFile)); err != nil {
		return nil
	}
	out, err := labCommand(ns, bin, dir, "status")
	if err != nil {
		return nil
	}
	var s struct {
		Network *apiv0.NetworkStatus `json:"network"`
	}
	if json.Unmarshal(out, &s) != nil {
		return nil
	}
	return s.Network
}
func labWait(t *testing.T, ctx context.Context, what string, fn func() bool) {
	t.Helper()
	timer := time.NewTicker(30 * time.Millisecond)
	defer timer.Stop()
	for {
		if fn() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(what, ctx.Err())
		case <-timer.C:
		}
	}
}
func labPath(ns *natlab.Namespace, bin, dir, peer string) apiv0.PeerPathStatus {
	if s := labStatus(ns, bin, dir); s != nil && s.Connectivity != nil {
		for _, p := range s.Connectivity.Paths {
			if p.PeerID == peer {
				return p
			}
		}
	}
	return apiv0.PeerPathStatus{}
}
func labAdd(t *testing.T, ns *natlab.Namespace, bin, dir, file string) string {
	t.Helper()
	out := labRun(t, ns, bin, dir, "add", "--raw", file)
	var event apiv0.Event
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		labCheck(t, json.Unmarshal(scanner.Bytes(), &event))
	}
	if event.Operation == nil || event.Operation.State != apiv0.StateSucceeded {
		t.Fatalf("add failed: %s", out)
	}
	var result apiv0.AddResult
	labCheck(t, json.Unmarshal(event.Operation.Result, &result))
	return result.CID
}

// This runs the actual CLI binary: two daemon processes behind different NATs,
// two public `nu relay` processes, CLI add/share/get, operator exact-endpoint
// probe grants and real node/swarm assembly. No raw qpath/disco fixture replaces
// the daemon. SIGKILL is abrupt relay loss, not a graceful QUIC close.
func TestDaemonNATShareUpgradeAndRelayFailover(t *testing.T) {
	if !natlab.IsolateTimeout(t, 90*time.Second) {
		return
	}
	daemonNATCase(t, natlab.EndpointIndependent, true)
}

func TestDaemonNATRelayMatrix(t *testing.T) {
	if !natlab.IsolateTimeout(t, 90*time.Second) {
		return
	}
	for _, mapping := range []natlab.Mapping{natlab.AddressDependent, natlab.AddressPortDependent, natlab.Kernel} {
		t.Run(string(mapping), func(t *testing.T) { daemonNATCase(t, mapping, false) })
	}
}

func daemonNATCase(t *testing.T, mapping natlab.Mapping, failover bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	t.Cleanup(cancel)
	bin := filepath.Join(t.TempDir(), "nu")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "../../cmd/nu")
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	if _, err := exec.LookPath("tc"); err != nil {
		t.Fatal("tc is required to hold an in-progress get", err)
	}
	lab, err := natlab.New(ctx)
	labCheck(t, err)
	t.Cleanup(lab.Close)
	internet, err := lab.Namespace("internet")
	labCheck(t, err)
	wan, err := internet.Segment("wan", "198.18.0.1/24")
	labCheck(t, err)
	var clients, routers, relays [2]*natlab.Namespace
	for i := range clients {
		routers[i], err = lab.Namespace("router")
		labCheck(t, err)
		clients[i], err = lab.Namespace("daemon")
		labCheck(t, err)
		relays[i], err = lab.Namespace("relay")
		labCheck(t, err)
		labCheck(t, wan.Attach(routers[i], "wan", fmt.Sprintf("198.18.0.%d/24", 20+10*i), ""))
		labCheck(t, wan.Attach(relays[i], "eth0", fmt.Sprintf("198.18.0.%d/24", 2+i), ""))
		lan, err := routers[i].Segment("lan", fmt.Sprintf("10.%d.0.1/24", i+1))
		labCheck(t, err)
		labCheck(t, lan.Attach(clients[i], "eth0", fmt.Sprintf("10.%d.0.2/24", i+1), fmt.Sprintf("10.%d.0.1", i+1)))
		_, err = routers[i].NAT(natlab.NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr(fmt.Sprintf("198.18.0.%d", 20+10*i)), Mapping: mapping, Filtering: natlab.FilterAddressPortDependent, Inside: []netip.AddrPort{netip.MustParseAddrPort(fmt.Sprintf("10.%d.0.2:4171", i+1))}, Destinations: []netip.AddrPort{netip.MustParseAddrPort("198.18.0.2:4000"), netip.MustParseAddrPort("198.18.0.3:4000"), netip.MustParseAddrPort(fmt.Sprintf("198.18.0.%d:4171", 30-10*i))}})
		labCheck(t, err)
		// Force the first get through relay, including after home loss. Direct
		// candidates are allowed by owner policy but packets cannot yet traverse.
		labCheck(t, routers[i].NFT(fmt.Sprintf("add table inet nodirect\nadd chain inet nodirect forward { type filter hook forward priority -10; policy accept; }\nadd rule inet nodirect forward ip daddr 198.18.0.%d udp dport 4171 drop\n", 30-10*i)))
	}
	var dirs, rdirs [2]string
	var keys, rkeys [2]*wire.NodeKey
	for i := range dirs {
		dirs[i] = filepath.Join(t.TempDir(), "node")
		rdirs[i] = filepath.Join(t.TempDir(), "relay")
		labRun(t, clients[i], bin, dirs[i], "init")
		labRun(t, relays[i], bin, rdirs[i], "init")
		keys[i], err = wire.LoadNodeKey(filepath.Join(dirs[i], repo.KeyFile))
		labCheck(t, err)
		rkeys[i], err = wire.LoadNodeKey(filepath.Join(rdirs[i], repo.KeyFile))
		labCheck(t, err)
	}
	relayHints := []string{}
	var procs [2]*daemonLabProcess
	for i := range relays {
		relayHints = append(relayHints, fmt.Sprintf("/ip4/198.18.0.%d/udp/4000/quic-v1/p2p/%s", i+2, rkeys[i].Public().PeerID()))
		procs[i] = labStart(t, relays[i], bin, rdirs[i], "relay", "--listen", fmt.Sprintf("198.18.0.%d:4000", i+2), "--admit", keys[0].Public().PeerID().String(), "--admit", keys[1].Public().PeerID().String())
	}
	var daemons [2]*daemonLabProcess
	for i := range clients {
		labConfig(t, dirs[i], "swarm.listen", labArray(fmt.Sprintf("/ip4/10.%d.0.2/udp/4171/quic-v1", i+1)))
		labConfig(t, dirs[i], "swarm.mdns", "false")
		labConfig(t, dirs[i], "swarm.relays", labArray(relayHints...))
		labConfig(t, dirs[i], "swarm.probe_allow", labArray(fmt.Sprintf("%s@198.18.0.%d:4171", keys[1-i].Public().PeerID(), 30-10*i)))
		labConfig(t, dirs[i], "api.listen", "127.0.0.1:0")
		labConfig(t, dirs[i], "gateway.listen", "127.0.0.1:0")
		daemons[i] = labStart(t, clients[i], bin, dirs[i], "daemon")
	}
	labWait(t, ctx, "both daemons reserving home and backup", func() bool {
		for i := range clients {
			select {
			case <-daemons[i].done:
				t.Fatalf("daemon %d exited during startup: %v", i, daemons[i].err)
			default:
			}
			s := labStatus(clients[i], bin, dirs[i])
			if s == nil || s.Connectivity == nil || s.Connectivity.Home == "" || s.Connectivity.Backup == "" {
				return false
			}
		}
		return true
	})
	count := 500000
	if !failover {
		count = 7000
	}
	data := bytes.Repeat([]byte("verified daemon relay-failover fixture\n"), count)
	input := filepath.Join(t.TempDir(), "input.bin")
	labCheck(t, os.WriteFile(input, data, 0600))
	root := labAdd(t, clients[0], bin, dirs[0], input)
	var shared apiv0.ShareLink
	labCheck(t, json.Unmarshal(labRun(t, clients[0], bin, dirs[0], "share", root), &shared))
	parsed, err := link.Parse(shared.Link)
	labCheck(t, err)
	circuits := 0
	for _, a := range parsed.Providers[0].Addrs {
		if host.IsCircuitAddress(a) {
			circuits++
		}
	}
	if !strings.HasPrefix(shared.Link, "nu://") || circuits != 2 {
		t.Fatal("share link omitted live relay hints", shared.Link)
	}
	if !failover {
		output := filepath.Join(t.TempDir(), "copy.bin")
		start := time.Now()
		labRun(t, clients[1], bin, dirs[1], "get", "-o", output, shared.Link)
		copied, err := os.ReadFile(output)
		labCheck(t, err)
		if sha256.Sum256(copied) != sha256.Sum256(data) {
			t.Fatal("matrix copied bytes differ")
		}
		path := labPath(clients[1], bin, dirs[1], keys[0].Public().PeerID().Base36())
		if path.Path != "relay" || !path.RelayWarm {
			t.Fatal("matrix did not use relay", path)
		}
		t.Logf("%s/APDF: real-daemon nu:// get succeeded in %v; SHA-256 verified %d bytes via %s", mapping, time.Since(start), len(data), path.Relay)
		return
	}
	// Shape the provider's physical socket so the first progress is demonstrably
	// partial. Remove shaping after SIGKILL; it is not a performance benchmark.
	_, err = clients[0].Run("tc", "qdisc", "add", "dev", "eth0", "root", "tbf", "rate", "16mbit", "burst", "32kb", "latency", "500ms")
	labCheck(t, err)
	output := filepath.Join(t.TempDir(), "copy.bin")
	get := clients[1].Command(bin, "--nu-path", dirs[1], "--json", "get", "-o", output, shared.Link)
	stdout, err := get.StdoutPipe()
	labCheck(t, err)
	stderr := new(bytes.Buffer)
	get.Stderr = stderr
	start := time.Now()
	labCheck(t, get.Start())
	defer func() { _ = get.Process.Kill() }()
	scanner := bufio.NewScanner(stdout)
	killed := false
	var final apiv0.Event
	var oldRelay string
	for scanner.Scan() {
		var event apiv0.Event
		labCheck(t, json.Unmarshal(scanner.Bytes(), &event))
		final = event
		if !killed && event.Progress != nil && event.Progress.Unit == "bytes" && event.Progress.Done > 0 && event.Progress.Done < int64(len(data)) {
			path := labPath(clients[1], bin, dirs[1], keys[0].Public().PeerID().Base36())
			if path.Path != "relay" {
				t.Fatal("get was not using relay before failure", path)
			}
			oldRelay = path.Relay
			provider := labStatus(clients[0], bin, dirs[0])
			if provider == nil || provider.Connectivity == nil || provider.Connectivity.Home != oldRelay {
				t.Fatal("active path was not the provider's selected home relay", path, provider)
			}
			for i := range procs {
				if rkeys[i].Public().PeerID().Base36() == oldRelay {
					labCheck(t, procs[i].cmd.Process.Kill())
					killed = true
				}
			}
			if !killed {
				t.Fatal("path used an unknown relay", path)
			}
			t.Logf("in-progress get: %d/%d verified bytes at %v; SIGKILL home relay %s", event.Progress.Done, len(data), time.Since(start), oldRelay)
			_, err = clients[0].Run("tc", "qdisc", "del", "dev", "eth0", "root")
			labCheck(t, err)
		}
	}
	labCheck(t, scanner.Err())
	if err := get.Wait(); err != nil {
		t.Fatalf("caller-visible get error: %v %s; last event %+v", err, stderr, final)
	}
	if !killed || final.Operation == nil || final.Operation.State != apiv0.StateSucceeded {
		t.Fatal("did not prove an in-progress successful get", killed, final)
	}
	copied, err := os.ReadFile(output)
	labCheck(t, err)
	if sha256.Sum256(copied) != sha256.Sum256(data) {
		t.Fatal("copied bytes differ")
	}
	path := labPath(clients[1], bin, dirs[1], keys[0].Public().PeerID().Base36())
	if path.Path != "relay" || path.Relay == oldRelay || !path.RelayWarm {
		t.Fatal("transfer did not use the backup relay", path)
	}
	t.Logf("get completed without caller retry/error in %v via backup %s; SHA-256 verified %d bytes", time.Since(start), path.Relay, len(data))
	// Open the EIM/APDF path and emit real route invalidations to restart probing.
	for i := range routers {
		labCheck(t, routers[i].NFT("delete table inet nodirect\n"))
		_, err = clients[i].Run("ip", "route", "replace", "default", "via", fmt.Sprintf("10.%d.0.1", i+1))
		labCheck(t, err)
	}
	directStart := time.Now()
	labWait(t, ctx, "authenticated owner disco direct upgrade", func() bool {
		return labPath(clients[0], bin, dirs[0], keys[1].Public().PeerID().Base36()).Path == "direct" && labPath(clients[1], bin, dirs[1], keys[0].Public().PeerID().Base36()).Path == "direct"
	})
	t.Logf("two NAT'd daemons upgraded to direct in %v: %+v", time.Since(directStart), labPath(clients[1], bin, dirs[1], keys[0].Public().PeerID().Base36()))
	// A fresh daemon with all UDP blocked must fail clearly, not claim success.
	// Use a new CID so the completed previous copy cannot satisfy the request.
	second := filepath.Join(t.TempDir(), "second.bin")
	labCheck(t, os.WriteFile(second, []byte("not previously held by recipient"), 0600))
	secondRoot := labAdd(t, clients[0], bin, dirs[0], second)
	labCheck(t, json.Unmarshal(labRun(t, clients[0], bin, dirs[0], "share", secondRoot), &shared))
	daemons[1].stop()
	labCheck(t, routers[1].NFT("add table inet blocked\nadd chain inet blocked forward { type filter hook forward priority -20; policy accept; }\nadd rule inet blocked forward meta l4proto udp drop\n"))
	daemons[1] = labStart(t, clients[1], bin, dirs[1], "daemon")
	labWait(t, ctx, "UDP-blocked daemon API", func() bool { s := labStatus(clients[1], bin, dirs[1]); return s != nil && s.Serving })
	blocked, err := labCommand(clients[1], bin, dirs[1], "get", "-o", filepath.Join(t.TempDir(), "blocked.bin"), shared.Link)
	if err == nil || !strings.Contains(strings.ToLower(string(blocked)), "udp") {
		t.Fatalf("UDP-blocked get did not fail clearly: %v %s", err, blocked)
	}
	t.Logf("UDP-blocked daemon get failed clearly: %s", bytes.TrimSpace(blocked))
}
