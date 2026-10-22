package kadinterop

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	goCID "github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	"github.com/whyrusleeping/nu/kad"
	nuhost "github.com/whyrusleeping/nu/p2p/host"
	nuproto "github.com/whyrusleeping/nu/p2p/proto"
	"github.com/whyrusleeping/nu/p2p/wire"
)

const kuboImage = "ipfs/kubo:v0.40.0@sha256:e08c602bf97f138a0ec5b42155b4fd4532852919250b11b4aadbea2bf42c0e10"

// This opt-in test exercises real Kubo routing commands over QUIC-v1 on an
// isolated Docker bridge: Nu ADD_PROVIDER -> Kubo findprovs, then Kubo provide
// -> Nu GET_PROVIDERS. It never joins public Amino.
func TestDockerKuboAminoProviderInterop(t *testing.T) {
	if os.Getenv("NU_DOCKER") != "1" {
		t.Skip("set NU_DOCKER=1 to run digest-pinned local Kubo provider interop")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if _, err := dockerOut(ctx, "info"); err != nil {
		t.Fatalf("Docker unavailable: %v", err)
	}
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	network := "nu-kad-" + stamp
	container := "nu-kad-kubo-" + stamp
	if _, err := dockerOut(ctx, "network", "create", "--driver", "bridge", "--subnet", "11.0.0.0/24", "--gateway", "11.0.0.1", network); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = dockerOut(context.Background(), "rm", "-f", container)
		_, _ = dockerOut(context.Background(), "network", "rm", network)
	}()
	gatewayText, err := dockerOut(ctx, "network", "inspect", "-f", "{{(index .IPAM.Config 0).Gateway}}", network)
	if err != nil {
		t.Fatal(err)
	}
	gateway := net.ParseIP(strings.TrimSpace(gatewayText))
	if gateway == nil || gateway.To4() == nil {
		t.Fatalf("bad bridge gateway %q", gatewayText)
	}
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	h, err := nuhost.New(nuhost.Config{Key: key, Listen: []string{gateway.To4().String() + ":0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	sessions, err := nuhost.NewSessions(h, nuhost.SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	core, err := nuproto.New(h, nuproto.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	localPort := udpPort(t, h.Addrs()[0].String())
	nuAddr, err := wire.ParseMultiaddr(fmt.Sprintf("/ip4/%s/udp/%d/quic-v1/p2p/%s", gateway.To4(), localPort, h.ID()), 1024)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapJSON, _ := json.Marshal([]string{nuAddr.String()})
	script := fmt.Sprintf("ipfs init --profile=test >/dev/null && ipfs config --json Bootstrap '%s' && ipfs config --json Addresses.Swarm '[\"/ip4/0.0.0.0/udp/4001/quic-v1\"]' && ipfs config --json Swarm.AddrFilters '[]' && ipfs config Routing.Type dhtserver && exec ipfs daemon --routing=dhtserver --migrate=true", bootstrapJSON)
	runArgs := []string{"run", "-d", "--name", container, "--network", network, "--env", "IPFS_TELEMETRY=off"}
	if os.Getenv("NU_KUBO_DEBUG") == "1" {
		runArgs = append(runArgs, "--env", "GOLOG_LOG_LEVEL=debug")
	}
	runArgs = append(runArgs, "--entrypoint", "/bin/sh", kuboImage, "-ec", script)
	if _, err = dockerOut(ctx, runArgs...); err != nil {
		t.Fatal(err)
	}
	if err = waitKubo(ctx, container); err != nil {
		t.Fatal(err)
	}
	peerText, err := dockerOut(ctx, "exec", container, "ipfs", "config", "Identity.PeerID")
	if err != nil {
		t.Fatal(err)
	}
	kuboID, err := wire.ParsePeerID(lastNonempty(peerText))
	if err != nil {
		t.Fatalf("parse Kubo PeerId from %q: %v", peerText, err)
	}
	ipText, err := dockerOut(ctx, "inspect", "-f", "{{(index .NetworkSettings.Networks \""+network+"\").IPAddress}}", container)
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(strings.TrimSpace(ipText))
	if ip == nil || ip.To4() == nil {
		t.Fatalf("bad Kubo container IP %q", ipText)
	}
	kuboAddr, err := wire.ParseMultiaddr(fmt.Sprintf("/ip4/%s/udp/4001/quic-v1/p2p/%s", ip.To4(), kuboID), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err = sessions.SetPeer(kuboID, []wire.Multiaddr{kuboAddr}); err != nil {
		t.Fatal(err)
	}
	svc, err := kad.New(ctx, kad.Config{Host: h, Sessions: sessions, PubliclyReachable: true, AllowPrivateAddresses: true, Bootstrap: []kad.Peer{{ID: kuboID, Addrs: [][]byte{kuboAddr.Bytes()}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	// Force both sides to learn one another before announcing.
	if _, err = sessions.Connect(ctx, kuboID); err != nil {
		t.Fatalf("Nu -> Kubo QUIC connect: %v", err)
	}
	if _, err = svc.FindNode(ctx, kuboID); err != nil {
		t.Logf("initial FIND_NODE (connection is already established): %v", err)
	}
	if _, err = dockerOut(ctx, "exec", container, "ipfs", "swarm", "connect", nuAddr.String()); err != nil {
		t.Fatalf("Kubo -> Nu QUIC connect: %v", err)
	}
	peerList, err := dockerOut(ctx, "exec", container, "ipfs", "swarm", "peers")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(peerList, h.ID().String()) {
		t.Fatalf("Kubo swarm did not retain Nu peer connection: %s", peerList)
	}
	kuboIdentify, err := dockerOut(ctx, "exec", container, "ipfs", "id", h.ID().String())
	if err != nil {
		t.Fatalf("Kubo identify Nu: %v", err)
	}
	var identified struct{ Protocols []string }
	identifyJSON := kuboIdentify
	if at := strings.LastIndex(kuboIdentify, "{\n\t\"ID\""); at >= 0 {
		identifyJSON = kuboIdentify[at:]
	}
	if err = json.Unmarshal([]byte(identifyJSON), &identified); err != nil {
		t.Fatalf("decode Kubo identify response %q: %v", kuboIdentify, err)
	}
	if !slices.Contains(identified.Protocols, kad.Protocol) {
		t.Fatalf("Nu server did not advertise Amino: %v", identified.Protocols)
	}
	routingType, err := dockerOut(ctx, "exec", container, "ipfs", "config", "Routing.Type")
	if err != nil || lastNonempty(routingType) != "dhtserver" {
		t.Fatalf("Kubo routing mode=%q err=%v", routingType, err)
	}
	if _, err = dockerOut(ctx, "exec", container, "ipfs", "routing", "findpeer", h.ID().String()); err != nil {
		t.Fatalf("Kubo FIND_PEER Nu: %v", err)
	}

	keyBytes := randomBLAKE3Key(t)
	cid := goCID.NewCidV1(goCID.Raw, mh.Multihash(keyBytes))
	if err = svc.Provide(ctx, keyBytes, []wire.Multiaddr{nuAddr}); err != nil {
		t.Fatalf("Nu provide: %v", err)
	}
	findOut, err := dockerOut(ctx, "exec", container, "ipfs", "routing", "findprovs", "--num-providers=5", cid.String())
	if err != nil {
		t.Fatalf("Kubo ipfs routing findprovs: %v\n%s", err, findOut)
	}
	if !strings.Contains(findOut, h.ID().String()) {
		t.Fatalf("Kubo findprovs did not return Nu provider %s:\n%s", h.ID(), findOut)
	}

	goCIDText, err := dockerOut(ctx, "exec", container, "sh", "-ec", "printf 'Nu Amino Kubo provider fixture\\n' >/tmp/nu-provider-fixture && ipfs add -q --raw-leaves --cid-version=1 --hash=blake3 /tmp/nu-provider-fixture")
	if err != nil {
		t.Fatalf("Kubo BLAKE3 fixture add: %v", err)
	}
	goCID, err := goCID.Decode(lastNonempty(goCIDText))
	if err != nil {
		t.Fatalf("parse Kubo BLAKE3 CID %q: %v", goCIDText, err)
	}
	goKey := []byte(goCID.Hash())
	if out, err := dockerOut(ctx, "exec", container, "ipfs", "routing", "provide", goCID.String()); err != nil {
		logs, _ := dockerOut(ctx, "logs", container)
		t.Fatalf("Kubo ipfs routing provide: %v\n%s\nKubo logs:\n%s", err, out, logs)
	}
	providers, err := svc.FindProviders(ctx, goKey)
	if err != nil {
		t.Fatalf("Nu GET_PROVIDERS for Kubo CID: %v", err)
	}
	found := false
	for _, p := range providers {
		if p.ID == kuboID {
			found = true
		}
	}
	if !found {
		t.Fatalf("Nu did not find Kubo provider %s (providers=%v)", kuboID, providers)
	}
	t.Logf("Kubo v0.40.0 QUIC-v1 Amino interop passed: Nu BLAKE3 provide found by Kubo; Kubo BLAKE3 provide found by Nu; Kubo peer=%s Nu peer=%s", kuboID, h.ID())
}

func dockerOut(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		return string(b), fmt.Errorf("docker %v: %w", args, err)
	}
	return string(b), nil
}
func waitKubo(ctx context.Context, name string) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		logs, err := dockerOut(ctx, "logs", name)
		if err == nil && strings.Contains(logs, "Daemon is ready") {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for Kubo readiness: %w; logs: %s", ctx.Err(), logs)
		case <-ticker.C:
		}
	}
}
func udpPort(t *testing.T, a string) int {
	t.Helper()
	parts := strings.Split(a, "/")
	for i, p := range parts {
		if p == "udp" && i+1 < len(parts) {
			var port int
			if _, err := fmt.Sscanf(parts[i+1], "%d", &port); err == nil {
				return port
			}
		}
	}
	t.Fatalf("no UDP port in %s", a)
	return 0
}
func randomBLAKE3Key(t *testing.T) []byte {
	t.Helper()
	payload := make([]byte, 32)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	mhBytes, err := mh.Encode(payload, 0x1e)
	if err != nil {
		t.Fatal(err)
	}
	return mhBytes
}
func lastNonempty(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}
