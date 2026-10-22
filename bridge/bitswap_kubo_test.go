package bridge

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/mr-tron/base58/base58"
	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type staticProviders []kad.Peer

func (p staticProviders) FindProviders(context.Context, []byte) ([]kad.Peer, error) { return p, nil }
func TestBitswapKuboQUIC(t *testing.T) {
	if os.Getenv("NU_DOCKER") != "1" {
		t.Skip("set NU_DOCKER=1 for opt-in Kubo Bitswap test")
	}
	dir := t.TempDir()
	const port = "44171"
	cmd := exec.Command("docker", "run", "-d", "--rm", "--network", "host", "--env", "GOLOG_LOG_LEVEL=bitswap=debug", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--entrypoint", "/bin/sh", "-v", dir+":/fixture", "-w", "/fixture", kuboImage, "-ec", `ipfs init --profile=test >/dev/null
ipfs config Addresses.Swarm --json '["/ip4/127.0.0.1/udp/44171/quic-v1"]'
ipfs config Bootstrap --json '[]'
head -c 1048576 /dev/urandom > payload
ipfs block put --format=raw --mhtype=sha2-256 payload > cid
ipfs pin add "$(cat cid)" >/dev/null
ipfs id -f='<id>' > peer
exec ipfs daemon
`)
	out, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("start Kubo: %v\n%s", e, out)
	}
	containerID := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", containerID).Run() })
	ready := false
	for i := 0; i < 300; i++ {
		cidBytes, cidErr := os.ReadFile(filepath.Join(dir, "cid"))
		peerBytes, peerErr := os.ReadFile(filepath.Join(dir, "peer"))
		if cidErr == nil && peerErr == nil && len(cidBytes) > 0 && len(peerBytes) > 0 {
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		logs, _ := exec.Command("docker", "logs", containerID).CombinedOutput()
		t.Fatalf("Kubo did not initialize fixture: %s", logs)
	}
	raw, e := os.ReadFile(filepath.Join(dir, "cid"))
	if e != nil {
		t.Fatal(e)
	}
	id, e := ParseIPFSCID(strings.TrimSpace(string(raw)))
	if e != nil {
		t.Fatal(e)
	}
	peerText, e := os.ReadFile(filepath.Join(dir, "peer"))
	if e != nil {
		t.Fatal(e)
	}
	peerBytes, e := base58.Decode(strings.TrimSpace(string(peerText)))
	if e != nil {
		t.Fatal(e)
	}
	peer, e := wire.PeerIDFromBytes(peerBytes)
	if e != nil {
		t.Fatal(e)
	}
	addr, e := wire.ParseMultiaddr(fmt.Sprintf("/ip4/127.0.0.1/udp/%s/quic-v1/p2p/%s", port, peer.String()), 1024)
	if e != nil {
		t.Fatal(e)
	}
	key, e := wire.GenerateNodeKey()
	if e != nil {
		t.Fatal(e)
	}
	h, e := host.New(host.Config{Key: key, Listen: []string{"127.0.0.1:0"}})
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	if proto, e := exec.Command("docker", "exec", containerID, "ipfs", "id", "-f=<protocols>").CombinedOutput(); e == nil {
		t.Logf("Kubo protocols: %s", proto)
	}
	src := NewBitswapSource(h, staticProviders{{ID: peer, Addrs: [][]byte{addr.Bytes()}}})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	got, e := src.GetBlock(ctx, id)
	if e != nil {
		log, _ := exec.Command("docker", "logs", containerID).CombinedOutput()
		t.Fatalf("Bitswap request failed: %v\nKubo: %s", e, log)
	}
	want, e := os.ReadFile(filepath.Join(dir, "payload"))
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != len(want) || sha256.Sum256(got) != sha256.Sum256(want) {
		t.Fatal("Bitswap block bytes differ")
	}
}
