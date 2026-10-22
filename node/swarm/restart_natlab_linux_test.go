//go:build linux && natlab

package swarm_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/natlab"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

func TestDaemonOneSidedRestart(t *testing.T) {
	if !natlab.IsolateTimeout(t, 180*time.Second) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 170*time.Second)
	defer cancel()
	bin := labBinary(t, ctx)
	for _, mode := range []string{"direct", "quic-relay", "wss-relay"} {
		t.Run(mode, func(t *testing.T) { restartDaemonCase(t, ctx, bin, mode) })
	}
}

func restartDaemonCase(t *testing.T, parent context.Context, bin, mode string) {
	ctx, cancel := context.WithTimeout(parent, 50*time.Second)
	defer cancel()
	lab, err := natlab.New(ctx)
	labCheck(t, err)
	t.Cleanup(lab.Close)
	bridge, err := lab.Namespace("bridge")
	labCheck(t, err)
	segment, err := bridge.Segment("wan", "198.18.0.1/24")
	labCheck(t, err)
	var ns [2]*natlab.Namespace
	var dirs [2]string
	var keys [2]*wire.NodeKey
	var daemons [2]*daemonLabProcess
	for i := range ns {
		ns[i], err = lab.Namespace("node")
		labCheck(t, err)
		labCheck(t, segment.Attach(ns[i], "eth0", fmt.Sprintf("198.18.0.%d/24", 10+i), ""))
		rule := fmt.Sprintf("ip daddr 198.18.0.%d udp dport 4171", 11-i)
		if mode == "quic-relay" {
			rule = "ip daddr 198.18.0.2 udp dport 443"
		}
		if mode == "wss-relay" {
			rule = "ip daddr 198.18.0.2 tcp dport 443"
		}
		labCheck(t, ns[i].NFT("add table inet onlypath\nadd chain inet onlypath output { type filter hook output priority 0; policy drop; }\nadd rule inet onlypath output oifname lo meta l4proto tcp accept\nadd rule inet onlypath output "+rule+" counter accept\n"))
		dirs[i] = filepath.Join(t.TempDir(), "node")
		labRun(t, ns[i], bin, dirs[i], "init")
		keys[i], err = wire.LoadNodeKey(filepath.Join(dirs[i], repo.KeyFile))
		labCheck(t, err)
	}
	// Exercise opposite deterministic QUIC roles across relay modes.
	wantLower := mode != "wss-relay"
	if (keys[0].Public().PeerID() < keys[1].Public().PeerID()) != wantLower {
		keys[0], keys[1] = keys[1], keys[0]
		for i := range keys {
			labCheck(t, os.WriteFile(filepath.Join(dirs[i], repo.KeyFile), keys[i].Marshal(), 0600))
		}
	}
	var relayAddr string
	if mode != "direct" {
		rns, err := lab.Namespace("relay")
		labCheck(t, err)
		labCheck(t, segment.Attach(rns, "eth0", "198.18.0.2/24", ""))
		rdir := filepath.Join(t.TempDir(), "relay")
		labRun(t, rns, bin, rdir, "init")
		rk, err := wire.LoadNodeKey(filepath.Join(rdir, repo.KeyFile))
		labCheck(t, err)
		relayAddr = "/ip4/198.18.0.2/udp/443/quic-v1/p2p/" + rk.Public().PeerID().String()
		args := []string{"relay", "--listen", "198.18.0.2:443", "--admit", keys[0].Public().PeerID().String(), "--admit", keys[1].Public().PeerID().String()}
		if mode == "wss-relay" {
			cert, key, _ := wssLabCertificate(t)
			t.Setenv("SSL_CERT_FILE", cert)
			relayAddr = "/ip4/198.18.0.2/tcp/443/tls/ws/p2p/" + rk.Public().PeerID().String()
			args = append(args, "--wss-listen", "198.18.0.2:443", "--wss-cert", cert, "--wss-key", key, "--advertise", relayAddr)
		}
		labStart(t, rns, bin, rdir, args...)
	}
	for i := range ns {
		labConfig(t, dirs[i], "swarm.listen", labArray(fmt.Sprintf("/ip4/198.18.0.%d/udp/4171/quic-v1", 10+i)))
		labConfig(t, dirs[i], "swarm.mdns", "false")
		labConfig(t, dirs[i], "swarm.dht", "off")
		if relayAddr != "" {
			labConfig(t, dirs[i], "swarm.relays", labArray(relayAddr))
		}
		labConfig(t, dirs[i], "api.listen", "127.0.0.1:0")
		labConfig(t, dirs[i], "gateway.listen", "127.0.0.1:0")
		daemons[i] = labStart(t, ns[i], bin, dirs[i], "daemon")
	}
	ready := func(i int) bool {
		s := labStatus(ns[i], bin, dirs[i])
		return s != nil && s.Serving && (mode == "direct" || s.Connectivity != nil && s.Connectivity.Home != "")
	}
	labWait(t, ctx, "daemon readiness", func() bool { return ready(0) && ready(1) })
	// Each fresh blob exercises a different interrupted side. Shaping ensures the
	// restart occurs during verified fetching rather than output extraction.
	for _, restart := range []int{0, 1} {
		data := bytes.Repeat([]byte(fmt.Sprintf("one-sided %s restart %d verified content\n", mode, restart)), 300000)
		input := filepath.Join(t.TempDir(), "input")
		labCheck(t, os.WriteFile(input, data, 0600))
		root := labAdd(t, ns[0], bin, dirs[0], input)
		var shared apiv0.ShareLink
		labCheck(t, json.Unmarshal(labRun(t, ns[0], bin, dirs[0], "share", root), &shared))
		_, err = ns[0].Run("tc", "qdisc", "add", "dev", "eth0", "root", "tbf", "rate", "8mbit", "burst", "32kb", "latency", "500ms")
		labCheck(t, err)
		output := filepath.Join(t.TempDir(), "copy")
		get := ns[1].Command(bin, "--nu-path", dirs[1], "--json", "get", "-o", output, shared.Link)
		stdout, err := get.StdoutPipe()
		labCheck(t, err)
		var stderr bytes.Buffer
		get.Stderr = &stderr
		labCheck(t, get.Start())
		timer := time.AfterFunc(25*time.Second, func() { _ = get.Process.Kill() })
		t.Cleanup(func() {
			timer.Stop()
			if get.ProcessState == nil {
				_ = get.Process.Kill()
				_ = get.Wait()
			}
		})
		scanner := bufio.NewScanner(stdout)
		interrupted := false
		var restarted time.Time
		for scanner.Scan() {
			var event apiv0.Event
			labCheck(t, json.Unmarshal(scanner.Bytes(), &event))
			if !interrupted && event.Progress != nil && event.Progress.Unit == "bytes" && event.Progress.Done >= 1<<20 && event.Progress.Done < int64(len(data)) {
				path := labPath(ns[1], bin, dirs[1], keys[0].Public().PeerID().Base36())
				expected := "relay"
				if mode == "direct" {
					expected = "direct-quic"
				}
				if path.Path != expected {
					t.Fatalf("wrong pre-restart path: %+v", path)
				}
				interrupted = true
				restarted = time.Now()
				if restart == 0 { // abrupt provider loss; the SAME CLI/get must succeed
					labCheck(t, daemons[0].cmd.Process.Kill())
					<-daemons[0].done
				} else {
					daemons[1].stop()
				}
				_, err = ns[0].Run("tc", "qdisc", "del", "dev", "eth0", "root")
				labCheck(t, err)
				if restart == 1 {
					break
				}
				daemons[0] = labStart(t, ns[0], bin, dirs[0], "daemon")
			}
		}
		if restart == 1 {
			_ = get.Process.Kill()
		}
		getErr := get.Wait()
		timer.Stop()
		if !interrupted {
			t.Fatalf("not interrupted: %v %s", getErr, &stderr)
		}
		if restart == 0 && getErr != nil {
			t.Fatalf("same get failed after provider restart: %v %s", getErr, &stderr)
		}
		if restart == 1 {
			id, err := cid.Parse(root)
			labCheck(t, err)
			st, err := store.Open(dirs[1], store.Options{})
			labCheck(t, err)
			_, held, complete, err := st.HeldRanges(id.Multihash())
			labCheck(t, err)
			labCheck(t, st.Close())
			if complete || len(held) == 0 {
				t.Fatal("no durable partial after recipient restart")
			}
			t.Logf("durable partial ranges before recipient restart: %v", held)
			daemons[1] = labStart(t, ns[1], bin, dirs[1], "daemon")
			labWait(t, ctx, "recipient ready", func() bool { return ready(1) })
			labRun(t, ns[1], bin, dirs[1], "get", "-o", output, shared.Link)
		}
		copied, err := os.ReadFile(output)
		labCheck(t, err)
		if sha256.Sum256(copied) != sha256.Sum256(data) {
			t.Fatal("restart changed bytes")
		}
		if elapsed := time.Since(restarted); elapsed > 15*time.Second {
			t.Fatalf("recovery exceeded 15s: %v", elapsed)
		} else {
			t.Logf("%s restart side=%d: %d verified bytes, recovered/completed in %v", mode, restart, len(data), elapsed)
		}
	}
}
