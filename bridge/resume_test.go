package bridge_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/bridge"
	"github.com/whyrusleeping/nu/bridge/ipfsfix"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
	"github.com/whyrusleeping/nu/store"
)

// resumeFixture is a directory of chunked files: 3 files × 8 raw leaves + 3 file nodes + root.
func resumeFixture() (*ipfsfix.DAG, bridge.IPFSCID) {
	rng := rand.New(rand.NewSource(42))
	files := map[string][]byte{}
	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		b := make([]byte, 8<<10)
		rng.Read(b)
		files[name] = b
	}
	d := ipfsfix.New()
	return d, d.Files(files, 1<<10)
}

const resumeKillAfter = 10

// A Bitswap import killed (SIGKILL) mid-transfer resumes from its verified checkpoints: no
// checkpointed block is requested again and the resumed import produces the same Nu tree.
func TestBitswapImportResumesAfterKill(t *testing.T) {
	dag, root := resumeFixture()
	if dir := os.Getenv("NU_BRIDGE_KILL_DIR"); dir != "" {
		served := 0
		var mu sync.Mutex
		runBitswapImport(t, dag, root, dir, func(bridge.IPFSCID) bool {
			mu.Lock()
			served++
			stall := served > resumeKillAfter
			mu.Unlock()
			if stall {
				fmt.Println("midway")
				select {} // the provider stalls; the parent kills this process
			}
			return true
		})
		t.Fatal("import finished before the kill")
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestBitswapImportResumesAfterKill$")
	cmd.Env = append(os.Environ(), "NU_BRIDGE_KILL_DIR="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() && sc.Text() != "midway" {
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	before := checkpointed(t, filepath.Join(dir, "blocks"))
	if len(before) != resumeKillAfter {
		t.Fatalf("%d verified blocks checkpointed before the kill, want %d", len(before), resumeKillAfter)
	}
	var mu sync.Mutex
	var again []string
	requested := 0
	res := runBitswapImport(t, dag, root, dir, func(c bridge.IPFSCID) bool {
		mu.Lock()
		defer mu.Unlock()
		requested++
		if before[checkpointName(c)] {
			again = append(again, c.String())
		}
		return true
	})
	if len(again) > 0 {
		t.Fatalf("resumed import re-requested verified blocks: %v", again)
	}
	if total := len(dag.Reachable(root)); requested != total-resumeKillAfter {
		t.Fatalf("resume requested %d blocks, want %d", requested, total-resumeKillAfter)
	}
	want, err := bridge.Import(context.Background(), root, mapSource(dag.Blocks), tree.HashSink{}, mst.MemStore{}, bridge.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Root != want.Root || res.Files != 3 || res.Bytes != 3*8<<10 {
		t.Fatalf("resumed tree %s (%d files, %d bytes), want %s", res.Root, res.Files, res.Bytes, want.Root)
	}
}

// runBitswapImport serves dag from one Nu host and imports root over Bitswap on another,
// checkpointing verified blocks under dir/blocks into the store at dir/store.
func runBitswapImport(t *testing.T, dag *ipfsfix.DAG, root bridge.IPFSCID, dir string, serve func(bridge.IPFSCID) bool) bridge.ImportResult {
	t.Helper()
	server, client := newHost(t), newHost(t)
	if err := dag.ServeBitswap(server, serve); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "store"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := bridge.NewBitswapSource(client, provider{peerOf(t, server)})
	defer src.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := bridge.Import(ctx, root, bridge.CheckpointSource{Dir: filepath.Join(dir, "blocks"), Upstream: src}, st, st, bridge.ImportOptions{Source: "bitswap"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func newHost(t *testing.T) *host.Host {
	t.Helper()
	key, err := wire.GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	h, err := host.New(host.Config{Key: key, Listen: []string{"127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func peerOf(t *testing.T, h *host.Host) kad.Peer {
	t.Helper()
	text, err := h.Addrs()[0].Text()
	if err != nil {
		t.Fatal(err)
	}
	a, err := wire.ParseMultiaddr(text+"/p2p/"+h.ID().String(), 2048)
	if err != nil {
		t.Fatal(err)
	}
	return kad.Peer{ID: h.ID(), Addrs: [][]byte{a.Bytes()}}
}

type provider struct{ p kad.Peer }

func (p provider) FindProviders(context.Context, []byte) ([]kad.Peer, error) {
	return []kad.Peer{p.p}, nil
}

type mapSource map[bridge.IPFSCID][]byte

func (m mapSource) GetBlock(_ context.Context, c bridge.IPFSCID) ([]byte, error) {
	if b, ok := m[c]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("missing %s", c)
}

func checkpointName(c bridge.IPFSCID) string {
	k := sha256.Sum256(c.Bytes())
	return hex.EncodeToString(k[:]) + ".block"
}

func checkpointed(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".block") {
			out[e.Name()] = true
		}
	}
	return out
}
