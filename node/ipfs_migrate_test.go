package node

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/bridge"
	"github.com/whyrusleeping/nu/bridge/ipfsfix"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// migrationFixture is a directory of three chunked files (3 × 8 raw leaves, 3 file nodes, a
// root) plus extra entries the caller may add.
func migrationFixture(seed int64, extra map[string]bridge.IPFSCID, d *ipfsfix.DAG) (bridge.IPFSCID, map[string][]byte) {
	rng := rand.New(rand.NewSource(seed))
	files := map[string][]byte{}
	entries := map[string]bridge.IPFSCID{}
	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		b := make([]byte, 8<<10)
		rng.Read(b)
		files[name] = b
		entries[name] = d.File(b, 1<<10)
	}
	for k, v := range extra {
		entries[k] = v
	}
	return d.Dir(entries), files
}

func plannedRoot(t *testing.T, d *ipfsfix.DAG, root bridge.IPFSCID) string {
	t.Helper()
	res, err := bridge.Import(context.Background(), root, blockMap(d.Blocks), tree.HashSink{}, mst.MemStore{}, bridge.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return res.Root.String()
}

type blockMap map[bridge.IPFSCID][]byte

func (m blockMap) GetBlock(_ context.Context, c bridge.IPFSCID) ([]byte, error) {
	if b, ok := m[c]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("missing %s", c)
}

type hookSource struct {
	bridge.BlockSource
	get func(bridge.IPFSCID)
}

func (h hookSource) GetBlock(ctx context.Context, c bridge.IPFSCID) ([]byte, error) {
	h.get(c)
	return h.BlockSource.GetBlock(ctx, c)
}

const killAfter = 10

// A CAR import whose process is SIGKILLed mid-conversion is recovered by the next start under
// the same operation ID; no block verified before the kill is read from the CAR again, and
// the resumed import yields the planned Nu tree.
func TestIPFSImportResumesAfterKill(t *testing.T) {
	d := ipfsfix.New()
	root, files := migrationFixture(7, nil, d)
	if dir := os.Getenv("NU_IPFS_KILL_DIR"); dir != "" {
		printed := make(chan struct{})
		var mu sync.Mutex
		served := 0
		ipfsSourceHook = func(s bridge.BlockSource) bridge.BlockSource {
			return hookSource{s, func(bridge.IPFSCID) {
				mu.Lock()
				served++
				stall := served > killAfter
				mu.Unlock()
				if stall {
					<-printed
					fmt.Println("midway")
					select {}
				}
			}}
		}
		r, _, err := repo.Open(filepath.Join(dir, "nu"))
		if err != nil {
			t.Fatal(err)
		}
		n, err := NewLocal(r)
		if err != nil {
			t.Fatal(err)
		}
		o, err := n.ImportIPFS(context.Background(), apiv0.IPFSImportRequest{Root: root.String(), CAR: filepath.Join(dir, "root.car")})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("op", o.ID)
		close(printed)
		select {}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "root.car"), d.CAR(root), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestIPFSImportResumesAfterKill$")
	cmd.Env = append(os.Environ(), "NU_IPFS_KILL_DIR="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var id string
	sc := bufio.NewScanner(out)
	for sc.Scan() && sc.Text() != "midway" {
		if v, ok := strings.CutPrefix(sc.Text(), "op "); ok {
			id = v
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if id == "" {
		t.Fatal("child did not start the import")
	}
	r, _, err := repo.Open(filepath.Join(dir, "nu"))
	if err != nil {
		t.Fatal(err)
	}
	blocksDir := filepath.Join(r.Root(), "imports", id, "blocks")
	before := map[string]bool{}
	entries, _ := os.ReadDir(blocksDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".block") {
			before[e.Name()] = true
		}
	}
	if len(before) != killAfter {
		t.Fatalf("%d checkpointed blocks at the kill, want %d", len(before), killAfter)
	}
	var mu sync.Mutex
	var again []string
	requested := 0
	ipfsSourceHook = func(s bridge.BlockSource) bridge.BlockSource {
		return hookSource{s, func(c bridge.IPFSCID) {
			mu.Lock()
			defer mu.Unlock()
			requested++
			k := sha256.Sum256(c.Bytes())
			if before[hex.EncodeToString(k[:])+".block"] {
				again = append(again, c.String())
			}
		}}
	}
	t.Cleanup(func() { ipfsSourceHook = nil })
	n, err := NewLocal(r) // recovery restarts the job
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	final, err := n.WaitOperation(context.Background(), id, 10*time.Second)
	if err != nil || final.State != apiv0.StateSucceeded {
		t.Fatalf("resumed operation: %+v %v", final, err)
	}
	var res apiv0.IPFSImportResult
	if err := json.Unmarshal(final.Result, &res); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(again) > 0 {
		t.Fatalf("resume re-read verified blocks from the source: %v", again)
	}
	if total := len(d.Reachable(root)); requested != total-killAfter {
		t.Fatalf("resume requested %d blocks, want %d", requested, total-killAfter)
	}
	if res.Root != plannedRoot(t, d, root) {
		t.Fatalf("resumed root %s differs from the planned tree", res.Root)
	}
	rc, err := n.Cat(context.Background(), res.Root+"/b.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != string(files["b.bin"]) {
		t.Fatal("resumed file bytes differ")
	}
}

// A pinset import reconciles every inventory entry (imported, missing, unsupported, skipped,
// failed) after a dry run that changes nothing, and never modifies its sources.
func TestIPFSPinsetReconciliation(t *testing.T) {
	r, _, err := repo.Open(filepath.Join(t.TempDir(), "nu"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := NewLocal(r)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	d := ipfsfix.New()
	good, _ := migrationFixture(1, map[string]bridge.IPFSCID{"link": d.Symlink("a.bin")}, d)
	partial, _ := migrationFixture(2, nil, d)
	symlink := d.Symlink("elsewhere")
	noCAR, _ := migrationFixture(3, nil, d)
	corrupt, _ := migrationFixture(4, nil, d)
	cars := t.TempDir()
	write := func(c bridge.IPFSCID, b []byte) {
		if err := os.WriteFile(filepath.Join(cars, c.String()+".car"), b, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	write(good, d.CAR(good))
	write(symlink, d.CAR(symlink))
	bad := d.CAR(corrupt)
	bad[len(bad)-1] ^= 1
	write(corrupt, bad)
	leaf := d.Reachable(partial)[len(d.Reachable(partial))-1]
	saved := d.Blocks[leaf]
	delete(d.Blocks, leaf)
	write(partial, d.CAR(partial))
	d.Blocks[leaf] = saved
	keys := map[string]any{}
	for _, c := range []bridge.IPFSCID{good, partial, symlink, noCAR, corrupt} {
		keys[c.String()] = map[string]string{"Type": "recursive", "Name": ""}
	}
	keys[d.Raw([]byte("direct")).String()] = map[string]string{"Type": "direct"}
	inv, _ := json.Marshal(map[string]any{"Keys": keys})
	pins := filepath.Join(t.TempDir(), "pins.json")
	if err := os.WriteFile(pins, inv, 0o400); err != nil {
		t.Fatal(err)
	}
	sourceDigest := func() string {
		h := sha256.New()
		for _, p := range append([]string{pins}, globAll(t, cars)...) {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(h, "%s %x\n", p, sha256.Sum256(b))
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	sourceBefore := sourceDigest()
	statsBefore, _ := n.Store.Stats()

	want := map[string]string{good.String(): apiv0.IPFSPlanned, partial.String(): apiv0.IPFSMissing, symlink.String(): apiv0.IPFSUnsupported,
		noCAR.String(): apiv0.IPFSMissing, corrupt.String(): apiv0.IPFSFailed}
	dry := runPinset(t, n, apiv0.IPFSImportRequest{Pins: pins, CARDir: cars, DryRun: true})
	checkPinset(t, dry, want)
	if statsAfter, _ := n.Store.Stats(); statsAfter.Objects != statsBefore.Objects || statsAfter.Pins != statsBefore.Pins {
		t.Fatal("pinset dry run changed the store")
	}
	if dry.BlobEstimateFitsHeadroom == nil || dry.EstimatedPhysicalBytes == 0 {
		t.Fatalf("dry run lacks a headroom estimate: %+v", dry)
	}

	want[good.String()] = apiv0.IPFSImported
	res := runPinset(t, n, apiv0.IPFSImportRequest{Pins: pins, CARDir: cars})
	checkPinset(t, res, want)
	for _, rr := range res.Roots {
		switch rr.IPFSRoot {
		case good.String():
			if rr.Root != plannedRoot(t, d, good) || rr.Root != dryRoot(dry, good.String()) || len(rr.Transformations) == 0 {
				t.Fatalf("imported root %+v does not match its plan or lacks the symlink transformation", rr)
			}
		case partial.String():
			if len(rr.Missing) != 1 || rr.Missing[0].CID != leaf.String() {
				t.Fatalf("partial root does not name its missing block: %+v", rr)
			}
		}
	}
	if sourceDigest() != sourceBefore {
		t.Fatal("pinset import modified its source files")
	}
}

func globAll(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func dryRoot(res apiv0.IPFSPinsetResult, ipfs string) string {
	for _, r := range res.Roots {
		if r.IPFSRoot == ipfs {
			return r.Root
		}
	}
	return ""
}

func runPinset(t *testing.T, n *Local, req apiv0.IPFSImportRequest) apiv0.IPFSPinsetResult {
	t.Helper()
	o, err := n.ImportIPFS(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	final, err := n.WaitOperation(context.Background(), o.ID, 10*time.Second)
	if err != nil || final.State != apiv0.StateSucceeded {
		t.Fatalf("pinset operation: %+v %v", final, err)
	}
	var res apiv0.IPFSPinsetResult
	if err := json.Unmarshal(final.Result, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func checkPinset(t *testing.T, res apiv0.IPFSPinsetResult, want map[string]string) {
	t.Helper()
	if res.Pins != len(want)+1 || len(res.Roots) != res.Pins {
		t.Fatalf("report covers %d of %d pins", len(res.Roots), res.Pins)
	}
	for _, r := range res.Roots {
		w, ok := want[r.IPFSRoot]
		if !ok {
			w = apiv0.IPFSSkipped
		}
		if r.Status != w {
			t.Errorf("%s: status %s (%s), want %s", r.IPFSRoot, r.Status, r.Error, w)
		}
	}
}

// fakeNet is just enough network for the Bitswap source: a host and a fixed provider.
type fakeNet struct {
	h *host.Host
	p kad.Peer
}

func (f fakeNet) Host() *host.Host                                             { return f.h }
func (f fakeNet) Connect(context.Context, wire.PeerID, []wire.Multiaddr) error { return nil }
func (f fakeNet) Addrs() []wire.Multiaddr                                      { return f.h.Addrs() }
func (f fakeNet) Sessions() *host.Sessions                                     { return nil }
func (f fakeNet) FindProviders(context.Context, []byte) ([]kad.Peer, error) {
	return []kad.Peer{f.p}, nil
}

// A network dry run walks structure only (no raw leaf or file chunk is requested), reports
// counts, declared bytes, transformations, missing blocks and the store headroom, and writes
// nothing.
func TestIPFSNetworkDryRun(t *testing.T) {
	r, _, err := repo.Open(filepath.Join(t.TempDir(), "nu"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := NewLocal(r)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	d := ipfsfix.New()
	lost := d.File(make([]byte, 4<<10), 1<<10)
	root, _ := migrationFixture(5, map[string]bridge.IPFSCID{"link": d.Symlink("a.bin"), "lost": lost}, d)
	server, client := testHost(t), testHost(t)
	var mu sync.Mutex
	var wanted []bridge.IPFSCID
	if err := d.ServeBitswap(server, func(c bridge.IPFSCID) bool {
		mu.Lock()
		defer mu.Unlock()
		wanted = append(wanted, c)
		return c != lost
	}); err != nil {
		t.Fatal(err)
	}
	text, _ := server.Addrs()[0].Text()
	addr, _ := wire.ParseMultiaddr(text+"/p2p/"+server.ID().String(), 2048)
	if err := n.AttachNetwork(fakeNet{h: client, p: kad.Peer{ID: server.ID(), Addrs: [][]byte{addr.Bytes()}}}); err != nil {
		t.Fatal(err)
	}
	before, _ := n.Store.Stats()
	o, err := n.ImportIPFS(context.Background(), apiv0.IPFSImportRequest{Root: root.String(), DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	final, err := n.WaitOperation(context.Background(), o.ID, 10*time.Second)
	if err != nil || final.State != apiv0.StateSucceeded {
		t.Fatalf("dry run: %+v %v", final, err)
	}
	var res apiv0.IPFSImportResult
	if err := json.Unmarshal(final.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.Complete == nil || *res.Complete || len(res.Missing) != 1 || res.Missing[0].Path != "lost" {
		t.Fatalf("missing structure not reported: %+v", res)
	}
	if res.Files != 3 || res.Bytes != 3*8<<10 || res.Directories != 1 || res.Symlinks != 1 || len(res.Transformations) != 1 {
		t.Fatalf("inventory counts: %+v", res)
	}
	if res.BlobEstimateFitsHeadroom == nil || res.EstimatedPhysicalBytes < res.Bytes || res.Root != "" {
		t.Fatalf("estimate/plan: %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, c := range wanted {
		if c.Codec == 0x55 {
			t.Fatalf("structure-only walk requested raw leaf %s", c)
		}
	}
	if after, _ := n.Store.Stats(); after.Objects != before.Objects || after.Pins != before.Pins {
		t.Fatal("network dry run changed the store")
	}
}

func testHost(t *testing.T) *host.Host {
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
