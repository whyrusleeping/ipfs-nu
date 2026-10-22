//go:build linux && natlab

package swarm_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/natlab"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// Test-only proxy process, in its own network namespace. It can tunnel only the
// declared relay, or refuse CONNECT. Never used by production code.
func TestWSSCONNECTHelper(t *testing.T) {
	mode := os.Getenv("NU_WSS_PROXY_HELPER")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	srv := &http.Server{Addr: "198.18.0.3:443", ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "deny" || r.Method != http.MethodConnect || r.Host != "198.18.0.2:443" {
			http.Error(w, "CONNECT refused", http.StatusForbidden)
			return
		}
		upstream, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			http.Error(w, "relay unavailable", 502)
			return
		}
		client, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		defer client.Close()
		defer upstream.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, rw); upstream.Close(); close(done) }()
		_, _ = io.Copy(client, upstream)
		client.Close()
		<-done
	})}
	if err := srv.ListenAndServe(); err != nil {
		t.Fatal(err)
	}
}

func wssLabCertificate(t *testing.T) (string, string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	labCheck(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("198.18.0.2")}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	labCheck(t, err)
	private, err := x509.MarshalECPrivateKey(key)
	labCheck(t, err)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	labCheck(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))
	labCheck(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}), 0600))
	return certFile, keyFile, fmt.Sprintf("%x", sha256.Sum256(der))
}

func TestDaemonWSSUDPBlocked(t *testing.T) {
	if !natlab.IsolateTimeout(t, 210*time.Second) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Second)
	defer cancel()
	bin := labBinary(t, ctx)
	for _, proxy := range []bool{false, true} {
		t.Run(fmt.Sprintf("CONNECT=%t", proxy), func(t *testing.T) { daemonWSSCase(t, ctx, bin, proxy) })
	}
}

func daemonWSSCase(t *testing.T, parent context.Context, bin string, proxy bool) {
	ctx, cancel := context.WithTimeout(parent, 85*time.Second)
	defer cancel()
	lab, err := natlab.New(ctx)
	labCheck(t, err)
	t.Cleanup(lab.Close)
	bridge, err := lab.Namespace("internet")
	labCheck(t, err)
	segment, err := bridge.Segment("wan", "198.18.0.1/24")
	labCheck(t, err)
	relayNS, err := lab.Namespace("relay")
	labCheck(t, err)
	labCheck(t, segment.Attach(relayNS, "eth0", "198.18.0.2/24", ""))
	var clients [2]*natlab.Namespace
	var dirs [2]string
	var keys [2]*wire.NodeKey
	for i := range clients {
		clients[i], err = lab.Namespace("node")
		labCheck(t, err)
		labCheck(t, segment.Attach(clients[i], "eth0", fmt.Sprintf("198.18.0.%d/24", 10+i), ""))
		allowed := "198.18.0.2"
		if proxy {
			allowed = "198.18.0.3"
		}
		// UDP is dropped even on loopback; only local CLI/API TCP and the one
		// declared TCP/443 destination can leave either daemon namespace.
		labCheck(t, clients[i].NFT(fmt.Sprintf("add table inet wssonly\nadd chain inet wssonly output { type filter hook output priority 0; policy drop; }\nadd rule inet wssonly output meta l4proto udp counter drop\nadd rule inet wssonly output oifname lo accept\nadd rule inet wssonly output ip daddr %s tcp dport 443 counter accept\n", allowed)))
		dirs[i] = filepath.Join(t.TempDir(), "node")
		labRun(t, clients[i], bin, dirs[i], "init")
		keys[i], err = wire.LoadNodeKey(filepath.Join(dirs[i], repo.KeyFile))
		labCheck(t, err)
	}
	var proxyNS *natlab.Namespace
	var proxyCmd *exec.Cmd
	startProxy := func(mode string) {
		proxyCmd = proxyNS.Command(os.Args[0], "-test.run=^TestWSSCONNECTHelper$", "-test.timeout=100s")
		proxyCmd.Env = append(os.Environ(), "NU_WSS_PROXY_HELPER="+mode)
		labCheck(t, proxyCmd.Start())
	}
	stopProxy := func() {
		if proxyCmd != nil {
			_ = proxyCmd.Process.Kill()
			_ = proxyCmd.Wait()
			proxyCmd = nil
		}
	}
	if proxy {
		proxyNS, err = lab.Namespace("proxy")
		labCheck(t, err)
		labCheck(t, segment.Attach(proxyNS, "eth0", "198.18.0.3/24", ""))
		startProxy("tunnel")
		t.Cleanup(stopProxy)
	}
	cert, key, pin := wssLabCertificate(t)
	t.Setenv("SSL_CERT_FILE", cert)
	rdir := filepath.Join(t.TempDir(), "relay")
	labRun(t, relayNS, bin, rdir, "init")
	relayKey, err := wire.LoadNodeKey(filepath.Join(rdir, repo.KeyFile))
	labCheck(t, err)
	rid := relayKey.Public().PeerID().String()
	wssAddr := "/ip4/198.18.0.2/tcp/443/tls/ws/p2p/" + rid
	udpAddr := "/ip4/198.18.0.2/udp/443/quic-v1/p2p/" + rid
	labStart(t, relayNS, bin, rdir, "relay", "--listen", "198.18.0.2:443", "--wss-listen", "198.18.0.2:443", "--wss-cert", cert, "--wss-key", key, "--advertise", wssAddr, "--admit", keys[0].Public().PeerID().String(), "--admit", keys[1].Public().PeerID().String())
	var daemons [2]*daemonLabProcess
	for i := range clients {
		labConfig(t, dirs[i], "swarm.listen", labArray(fmt.Sprintf("/ip4/198.18.0.%d/udp/4171/quic-v1", 10+i)))
		labConfig(t, dirs[i], "swarm.mdns", "false")
		labConfig(t, dirs[i], "swarm.dht", "off")
		labConfig(t, dirs[i], "swarm.relays", labArray(udpAddr, wssAddr))
		labConfig(t, dirs[i], "swarm.relay_cert_pins", labArray(rid+"@"+pin))
		if proxy {
			labConfig(t, dirs[i], "swarm.proxy", "http://198.18.0.3:443")
		}
		labConfig(t, dirs[i], "api.listen", "127.0.0.1:0")
		labConfig(t, dirs[i], "gateway.listen", "127.0.0.1:0")
		daemons[i] = labStart(t, clients[i], bin, dirs[i], "daemon")
	}
	ready := func(i int) bool {
		s := labStatus(clients[i], bin, dirs[i])
		return s != nil && s.Connectivity != nil && s.Connectivity.Home != "" && len(s.Connectivity.Relays) == 1 && s.Connectivity.Relays[0].Transport == "wss" && s.Connectivity.Relays[0].UDPError != ""
	}
	labWait(t, ctx, "both nodes independently fell back from blocked UDP to WSS", func() bool { return ready(0) && ready(1) })
	data := bytes.Repeat([]byte("verified resumable WSS daemon content\n"), 300000)
	input := filepath.Join(t.TempDir(), "input")
	labCheck(t, os.WriteFile(input, data, 0600))
	root := labAdd(t, clients[0], bin, dirs[0], input)
	var shared apiv0.ShareLink
	labCheck(t, json.Unmarshal(labRun(t, clients[0], bin, dirs[0], "share", root), &shared))
	// Hold the transfer long enough to interrupt only after verified partial data.
	_, err = clients[0].Run("tc", "qdisc", "add", "dev", "eth0", "root", "tbf", "rate", "8mbit", "burst", "32kb", "latency", "500ms")
	labCheck(t, err)
	output := filepath.Join(t.TempDir(), "copy")
	get := clients[1].Command(bin, "--nu-path", dirs[1], "--json", "get", "-o", output, shared.Link)
	stdout, err := get.StdoutPipe()
	labCheck(t, err)
	labCheck(t, get.Start())
	defer func() { _ = get.Process.Kill() }()
	scanner := bufio.NewScanner(stdout)
	interrupted := false
	for scanner.Scan() {
		var event apiv0.Event
		labCheck(t, json.Unmarshal(scanner.Bytes(), &event))
		if event.Progress != nil && event.Progress.Unit == "bytes" && event.Progress.Done > 0 && event.Progress.Done < int64(len(data)) {
			daemons[1].stop()
			interrupted = true
			break
		}
	}
	_ = get.Process.Kill()
	_ = get.Wait()
	if !interrupted {
		t.Fatal("no partial transfer to interrupt")
	}
	id, err := cid.Parse(root)
	labCheck(t, err)
	st, err := store.Open(dirs[1], store.Options{})
	labCheck(t, err)
	_, held, complete, err := st.HeldRanges(id.Multihash())
	labCheck(t, err)
	var heldBytes uint64
	for _, r := range held {
		_, n := r.Bytes(uint64(len(data)))
		heldBytes += n
	}
	labCheck(t, st.Close())
	if complete || heldBytes == 0 || heldBytes >= uint64(len(data)) {
		t.Fatalf("not a persisted partial: held=%v complete=%t", held, complete)
	}
	t.Logf("interrupted receiver daemon; %d verified bytes persisted", heldBytes)
	_, err = clients[0].Run("tc", "qdisc", "del", "dev", "eth0", "root")
	labCheck(t, err)
	// Only the recipient restarts; the provider must retire its stale inner session.
	daemons[1] = labStart(t, clients[1], bin, dirs[1], "daemon")
	labWait(t, ctx, "restarted WSS reservations", func() bool { return ready(0) && ready(1) })
	result, getErr := labCommand(clients[1], bin, dirs[1], "get", "-o", output, shared.Link)
	if getErr != nil {
		for i := range clients {
			t.Logf("node %d status: %s", i, labRun(t, clients[i], bin, dirs[i], "status"))
		}
		t.Fatalf("resumed get: %v %s", getErr, result)
	}
	resumed := false
	scanner = bufio.NewScanner(bytes.NewReader(result))
	for scanner.Scan() {
		var event apiv0.Event
		labCheck(t, json.Unmarshal(scanner.Bytes(), &event))
		if event.Progress != nil && event.Progress.Unit == "bytes" && event.Progress.Done >= int64(heldBytes) {
			resumed = true
		}
	}
	copied, err := os.ReadFile(output)
	labCheck(t, err)
	if !resumed || sha256.Sum256(copied) != sha256.Sum256(data) {
		t.Fatal("resumed copy differs")
	}
	path := labPath(clients[1], bin, dirs[1], keys[0].Public().PeerID().Base36())
	if path.Path != "relay" || !path.RelayWarm {
		t.Fatalf("not a relayed inner path: %+v", path)
	}
	t.Logf("CONNECT=%t: SHA-256 verified %d bytes after restart via WSS-only TCP/443", proxy, len(data))
	if proxy {
		daemons[1].stop()
		stopProxy()
		startProxy("deny")
		daemons[1] = labStart(t, clients[1], bin, dirs[1], "daemon")
		labWait(t, ctx, "CONNECT refusal reported", func() bool {
			s := labStatus(clients[1], bin, dirs[1])
			return s != nil && s.Connectivity != nil && len(s.Connectivity.Relays) == 1 && strings.Contains(s.Connectivity.Relays[0].LastError, "CONNECT denied: HTTP 403")
		})
		doctor, _ := labCommand(clients[1], bin, dirs[1], "doctor")
		if !strings.Contains(string(doctor), "CONNECT denied: HTTP 403") {
			t.Fatalf("doctor omitted proxy failure: %s", doctor)
		}
		t.Log("unsupported CONNECT-refusing middlebox: doctor reports HTTP 403")
	}
}
