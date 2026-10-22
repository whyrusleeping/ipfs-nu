//go:build linux && natlab

package swarm_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/link"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/nutest/slow"
	"github.com/whyrusleeping/nu/p2p/host"
	"github.com/whyrusleeping/nu/p2p/natlab"
	"github.com/whyrusleeping/nu/p2p/wire"
)

// TestMagicPreview is the automated part of 0029, not real-network or usability
// evidence. No parallel subtests: peak disk is ~30 GB, not N full provider stores.
func TestMagicPreview(t *testing.T) {
	slow.Skip(t)
	if !natlab.IsolateTimeout(t, 55*time.Minute) {
		return
	}
	size := int64(10_000_000_000) // decimal GB, explicitly not 10 GiB
	if s := os.Getenv("NU_MAGIC_BYTES"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		labCheck(t, err)
		if n < 64<<20 || n > size {
			t.Fatal("NU_MAGIC_BYTES must be 64 MiB..10 GB")
		}
		size = n
		t.Logf("SMOKE ONLY: %d bytes; not the 10 GB acceptance run", size)
	}
	bin := filepath.Join(t.TempDir(), "nu")
	out, err := exec.Command("go", "build", "-o", bin, "../../cmd/nu").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	for _, mapping := range []natlab.Mapping{natlab.EndpointIndependent, natlab.AddressPortDependent} {
		t.Run(string(mapping), func(t *testing.T) { magicCase(t, bin, size, mapping) })
	}
}

func magicDisk(t *testing.T, dir string) {
	t.Helper()
	out, err := exec.Command("df", "-h", dir).CombinedOutput()
	labCheck(t, err)
	t.Logf("disk guard:\n%s", out)
	var st syscall.Statfs_t
	labCheck(t, syscall.Statfs(dir, &st))
	if uint64(st.Bavail)*uint64(st.Bsize) < 40<<30 {
		t.Fatal("abort: less than 40 GiB free before big run")
	}
}

func magicAdd(t *testing.T, ns *natlab.Namespace, bin, dir string, args ...string) string {
	t.Helper()
	out := labRun(t, ns, bin, dir, append([]string{"add"}, args...)...)
	var last apiv0.Event
	scan := bufio.NewScanner(bytes.NewReader(out))
	for scan.Scan() {
		labCheck(t, json.Unmarshal(scan.Bytes(), &last))
	}
	labCheck(t, scan.Err())
	if last.Operation == nil || last.Operation.State != apiv0.StateSucceeded {
		t.Fatalf("add: %s", out)
	}
	var r apiv0.AddResult
	labCheck(t, json.Unmarshal(last.Operation.Result, &r))
	return r.CID
}
func magicShare(t *testing.T, ns *natlab.Namespace, bin, dir, root string) *link.Link {
	t.Helper()
	var r apiv0.ShareLink
	labCheck(t, json.Unmarshal(labRun(t, ns, bin, dir, "share", root), &r))
	l, err := link.Parse(r.Link)
	labCheck(t, err)
	if len(l.Providers) != 1 {
		t.Fatal("share missing provider", r.Link)
	}
	circuit := false
	for _, a := range l.Providers[0].Addrs {
		circuit = circuit || host.IsCircuitAddress(a)
	}
	if !circuit {
		t.Fatal("share missing live development relay", r.Link)
	}
	return l
}
func magicGet(t *testing.T, ns *natlab.Namespace, bin, dir, output, target string, progress func(int64)) error {
	t.Helper()
	cmd := ns.Command(bin, "--nu-path", dir, "--json", "get", "-o", output, target)
	stdout, err := cmd.StdoutPipe()
	labCheck(t, err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	labCheck(t, cmd.Start())
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	scan := bufio.NewScanner(stdout)
	stage := ""
	var last apiv0.Event
	for scan.Scan() {
		labCheck(t, json.Unmarshal(scan.Bytes(), &last))
		if last.Stage != "" {
			stage = last.Stage
		}
		if progress != nil && stage == "fetching" && last.Progress != nil && last.Progress.Unit == "bytes" {
			progress(last.Progress.Done)
		}
	}
	labCheck(t, scan.Err())
	err = cmd.Wait()
	if err != nil {
		return fmt.Errorf("get: %w: %s; last=%+v", err, &stderr, last)
	}
	if last.Operation == nil || last.Operation.State != apiv0.StateSucceeded {
		return fmt.Errorf("get terminal event: %+v", last)
	}
	return nil
}
func magicDigest(t *testing.T, file string, size int64) [32]byte {
	t.Helper()
	f, err := os.Open(file)
	labCheck(t, err)
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	labCheck(t, err)
	if n != size {
		t.Fatalf("%s: size %d != %d", file, n, size)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func magicCounter(t *testing.T, ns *natlab.Namespace) uint64 {
	t.Helper()
	out, err := ns.Run("nft", "-j", "list", "counter", "inet", "magic", "sent")
	labCheck(t, err)
	var result struct {
		NFT []struct {
			Counter *struct {
				Bytes uint64 `json:"bytes"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	labCheck(t, json.Unmarshal(out, &result))
	for _, row := range result.NFT {
		if row.Counter != nil {
			return row.Counter.Bytes
		}
	}
	t.Fatal("no nft byte counter", string(out))
	return 0
}

func magicCase(t *testing.T, bin string, size int64, mapping natlab.Mapping) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	t.Cleanup(cancel)
	work := t.TempDir()
	t.Cleanup(func() {
		// After daemons/namespace teardown, before t.TempDir removes the data.
		out, err := exec.Command("du", "-sk", work).CombinedOutput()
		if err != nil {
			t.Errorf("disk accounting: %v %s", err, out)
		}
		t.Logf("end-of-case allocated KiB (includes stores, backing/output files, outboards and tree): %s", out)
	})
	magicDisk(t, work)
	lab, err := natlab.New(ctx)
	labCheck(t, err)
	t.Cleanup(lab.Close)
	internet, err := lab.Namespace("internet")
	labCheck(t, err)
	wan, err := internet.Segment("wan", "198.18.0.1/24")
	labCheck(t, err)
	relay, err := lab.Namespace("relay")
	labCheck(t, err)
	labCheck(t, wan.Attach(relay, "eth0", "198.18.0.2/24", ""))
	var ns [4]*natlab.Namespace
	var dirs [4]string
	var keys [4]*wire.NodeKey
	var processes [4]*daemonLabProcess
	for i := range ns {
		router, err := lab.Namespace("router")
		labCheck(t, err)
		ns[i], err = lab.Namespace("laptop")
		labCheck(t, err)
		external := fmt.Sprintf("198.18.0.%d", 20+i)
		labCheck(t, wan.Attach(router, "wan", external+"/24", ""))
		lan, err := router.Segment("lan", fmt.Sprintf("10.%d.0.1/24", i+1))
		labCheck(t, err)
		inside := fmt.Sprintf("10.%d.0.2", i+1)
		labCheck(t, lan.Attach(ns[i], "eth0", inside+"/24", fmt.Sprintf("10.%d.0.1", i+1)))
		m, filter := natlab.EndpointIndependent, natlab.FilterEndpointIndependent
		if i == 1 && mapping == natlab.AddressPortDependent {
			m, filter = mapping, natlab.FilterAddressPortDependent
		}
		_, err = router.NAT(natlab.NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr(external), Mapping: m, Filtering: filter, Inside: []netip.AddrPort{netip.MustParseAddrPort(inside + ":4171")}, Destinations: []netip.AddrPort{netip.MustParseAddrPort("198.18.0.2:4000")}})
		labCheck(t, err)
		dirs[i] = filepath.Join(work, fmt.Sprintf("node%d", i))
		labRun(t, ns[i], bin, dirs[i], "init")
		keys[i], err = wire.LoadNodeKey(filepath.Join(dirs[i], repo.KeyFile))
		labCheck(t, err)
	}
	// A second, genuinely different gateway/subnet, attached but initially down.
	hotspot, err := lab.Namespace("hotspot")
	labCheck(t, err)
	labCheck(t, wan.Attach(hotspot, "wan", "198.18.0.90/24", ""))
	lan, err := hotspot.Segment("lan", "10.99.0.1/24")
	labCheck(t, err)
	labCheck(t, lan.Attach(ns[1], "eth1", "10.99.0.2/24", ""))
	labCheck(t, ns[1].Link("eth1", false))
	_, err = hotspot.NAT(natlab.NATConfig{WAN: "wan", LAN: "lan", ExternalIP: netip.MustParseAddr("198.18.0.90"), Mapping: natlab.AddressPortDependent, Filtering: natlab.FilterAddressPortDependent, Inside: []netip.AddrPort{netip.MustParseAddrPort("10.99.0.2:4171")}, Destinations: []netip.AddrPort{netip.MustParseAddrPort("198.18.0.2:4000")}})
	labCheck(t, err)
	rdir := filepath.Join(work, "relay")
	labRun(t, relay, bin, rdir, "init")
	rk, err := wire.LoadNodeKey(filepath.Join(rdir, repo.KeyFile))
	labCheck(t, err)
	args := []string{"relay", "--listen", "198.18.0.2:4000"}
	for _, k := range keys {
		args = append(args, "--admit", k.Public().PeerID().String())
	}
	labStart(t, relay, bin, rdir, args...)
	for i := range ns {
		// The ONLY laptop config change: no listen/mdns/probe/peer overrides.
		labConfig(t, dirs[i], "swarm.relays", labArray("/ip4/198.18.0.2/udp/4000/quic-v1/p2p/"+rk.Public().PeerID().String()))
		processes[i] = labStart(t, ns[i], bin, dirs[i], "daemon")
	}
	ready := func(i int) {
		labWait(t, ctx, "laptop relay reservation", func() bool {
			s := labStatus(ns[i], bin, dirs[i])
			return s != nil && s.Connectivity != nil && s.Connectivity.Home != ""
		})
	}
	for i := range ns {
		ready(i)
	}
	input := filepath.Join(work, "input.bin")
	f, err := os.Create(input)
	labCheck(t, err)
	labCheck(t, f.Truncate(size)) // sparse, no compression on the wire
	// Nonzero markers catch accidental truncation/zero-fill as well as wrong length.
	for _, off := range []int64{0, size / 2, size - 64} {
		_, err = f.WriteAt([]byte(fmt.Sprintf("0029 deterministic marker at %020d", off)), off)
		labCheck(t, err)
	}
	labCheck(t, f.Close())
	digest := magicDigest(t, input, size)
	start := time.Now()
	root := magicAdd(t, ns[0], bin, dirs[0], "--raw", "--nocopy", input)
	shared := magicShare(t, ns[0], bin, dirs[0], root)
	output := filepath.Join(work, "received.bin")
	bigShared, bigOutput, bigSize, bigDigest := shared, output, size, digest
	if mapping == natlab.EndpointIndependent {
		// First finish a full, unchanged cone-to-cone transfer. Network movement
		// is a separate 512 MiB transfer so it cannot substitute for scenario 1.
		magicDisk(t, work)
		labCheck(t, magicGet(t, ns[1], bin, dirs[1], output, shared.String(), nil))
		if magicDigest(t, output, size) != digest {
			t.Fatal("cone-to-cone SHA-256 mismatch")
		}
		if p := labPath(ns[1], bin, dirs[1], keys[0].Public().PeerID().Base36()); p.Path != "relay" {
			t.Fatal("expected declared relay", p)
		}
		t.Logf("SCENARIO 1 endpoint-independent: unchanged cone-to-cone %d bytes SHA-256=%x elapsed=%v", size, digest, time.Since(start))
		size = min(size, 512<<20)
		moving := filepath.Join(work, "moving.bin")
		mf, e := os.Create(moving)
		labCheck(t, e)
		labCheck(t, mf.Truncate(size))
		_, e = mf.WriteAt([]byte("0029 separate Wi-Fi to hotspot transfer"), size/2)
		labCheck(t, e)
		labCheck(t, mf.Close())
		digest = magicDigest(t, moving, size)
		moveRoot := magicAdd(t, ns[0], bin, dirs[0], "--raw", "--nocopy", moving)
		shared = magicShare(t, ns[0], bin, dirs[0], moveRoot)
		output = filepath.Join(work, "moving-out.bin")
	}
	changed := false
	var retained, last int64
	var before apiv0.PeerPathStatus
	if mapping == natlab.EndpointIndependent {
		_, err = ns[0].Run("tc", "qdisc", "add", "dev", "eth0", "root", "tbf", "rate", "64mbit", "burst", "64kb", "latency", "500ms")
		labCheck(t, err)
	}
	magicDisk(t, work)
	err = magicGet(t, ns[1], bin, dirs[1], output, shared.String(), func(done int64) {
		if done < last {
			t.Fatalf("verified progress regressed: %d < %d", done, last)
		}
		last = done
		if mapping == natlab.EndpointIndependent && !changed && done >= min(size/4, 256<<20) && done < size {
			before = labPath(ns[1], bin, dirs[1], keys[0].Public().PeerID().Base36())
			retained = done
			labCheck(t, ns[1].Link("eth0", false))
			labCheck(t, ns[1].Link("eth1", true))
			_, e := ns[1].Run("ip", "route", "replace", "default", "via", "10.99.0.1", "dev", "eth1")
			labCheck(t, e)
			_, e = ns[0].Run("tc", "qdisc", "del", "dev", "eth0", "root")
			labCheck(t, e)
			changed = true
			t.Logf("Wi-Fi -> hotspot at %d verified bytes; old path %+v", retained, before)
		}
	})
	// Scenario 3 is a live-recovery gate now: no daemon restart or CLI retry
	// may turn a failed movement into a passing full preview run.
	if err != nil {
		t.Fatal(err)
	}
	if magicDigest(t, output, size) != digest {
		t.Fatal("download SHA-256 mismatch")
	}
	path := labPath(ns[1], bin, dirs[1], keys[0].Public().PeerID().Base36())
	if path.Path != "relay" {
		t.Fatal("expected relay fallback without probe grants", path)
	}
	if mapping != natlab.EndpointIndependent {
		t.Logf("SCENARIO 1 %s: CLI share/get %d bytes SHA-256=%x elapsed=%v path=%s", mapping, size, digest, time.Since(start), path.Path)
	}
	if mapping == natlab.EndpointIndependent {
		if !changed || last < retained {
			t.Fatal("network-change progress not demonstrated")
		}
		// last_connected is a connection-acquisition timestamp, not an inner QUIC ID.
		// Do not infer inner connection survival from transparent transfer completion.
		t.Logf("SCENARIO 3: preserved >=%d verified bytes; no daemon/get retry; inner connection survival unmeasured (status timestamps before=%s after=%s)", retained, before.LastConnected, path.LastConnected)
	}
	if output != bigOutput {
		labCheck(t, os.Remove(output))
		// Restore the declared cone-to-cone fixture before the 10k-file tree.
		processes[0].stop()
		processes[1].stop()
		labCheck(t, ns[1].Link("eth1", false))
		labCheck(t, ns[1].Link("eth0", true))
		_, e := ns[1].Run("ip", "route", "replace", "default", "via", "10.2.0.1", "dev", "eth0")
		labCheck(t, e)
		for _, i := range []int{0, 1} {
			processes[i] = labStart(t, ns[i], bin, dirs[i], "daemon")
			ready(i)
		}
	}
	shared, output, size, digest = bigShared, bigOutput, bigSize, bigDigest
	// Identical source bytes on two real nodes, both blob and file-node hashes.
	other := magicAdd(t, ns[1], bin, dirs[1], "--raw", "--nocopy", output)
	if other != root {
		t.Fatalf("blob CIDs differ: %s %s", root, other)
	}
	aDir, bDir := filepath.Join(work, "same-a"), filepath.Join(work, "same-b")
	labCheck(t, os.Mkdir(aDir, 0700))
	labCheck(t, os.Mkdir(bDir, 0700))
	for _, d := range []string{aDir, bDir} {
		labCheck(t, os.WriteFile(filepath.Join(d, "same.txt"), []byte("same bytes, same name, same inferred text/plain MIME\n"), 0600))
	}
	a := magicAdd(t, ns[0], bin, dirs[0], filepath.Join(aDir, "same.txt"))
	b := magicAdd(t, ns[1], bin, dirs[1], filepath.Join(bDir, "same.txt"))
	if a != b {
		t.Fatalf("file-node CIDs differ: %s %s", a, b)
	}
	t.Logf("SCENARIO 4: identical blob CID %s; same name/MIME file-node CID %s", root, a)
	// 10k distinct files across 100 directories; compare every extracted byte/path.
	tree := filepath.Join(work, "tree")
	for i := 0; i < 10000; i++ {
		p := filepath.Join(tree, fmt.Sprintf("d%03d", i/100), fmt.Sprintf("f%05d.txt", i))
		labCheck(t, os.MkdirAll(filepath.Dir(p), 0700))
		labCheck(t, os.WriteFile(p, []byte(fmt.Sprintf("0029 tree file %d\n", i)), 0600))
	}
	start = time.Now()
	tr := magicAdd(t, ns[0], bin, dirs[0], "-r", tree)
	ts := magicShare(t, ns[0], bin, dirs[0], tr)
	treeOut := filepath.Join(work, "tree-out")
	labCheck(t, magicGet(t, ns[1], bin, dirs[1], treeOut, ts.String(), nil))
	files := 0
	labCheck(t, filepath.WalkDir(treeOut, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(treeOut, p)
		if e != nil {
			return e
		}
		got, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		want, e := os.ReadFile(filepath.Join(tree, rel))
		if e != nil {
			return e
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("tree mismatch %s", rel)
		}
		files++
		return nil
	}))
	if files != 10000 {
		t.Fatal("tree file count", files)
	}
	t.Logf("SCENARIO 1 %s: 10,000-file tree all paths/bytes verified in %v root=%s", mapping, time.Since(start), tr)
	if mapping != natlab.EndpointIndependent {
		return
	}
	// Replace recipient's owned blob with nocopy of its VERIFIED extraction, keeping
	// identity/config. Two provider repos reference that one immutable fetched file.
	// This saves 10 GB: never three full provider copies plus a new download/output.
	processes[1].stop()
	entries, err := os.ReadDir(dirs[1])
	labCheck(t, err)
	for _, e := range entries {
		if e.Name() != repo.KeyFile && e.Name() != repo.ConfigFile && e.Name() != repo.VersionFile {
			labCheck(t, os.RemoveAll(filepath.Join(dirs[1], e.Name())))
		}
	}
	processes[1] = labStart(t, ns[1], bin, dirs[1], "daemon")
	ready(1)
	for _, i := range []int{1, 2} {
		if magicAdd(t, ns[i], bin, dirs[i], "--raw", "--nocopy", output) != root {
			t.Fatal("provider nocopy CID mismatch")
		}
	}
	combined := &link.Link{CID: shared.CID}
	for i := 0; i < 3; i++ {
		one := magicShare(t, ns[i], bin, dirs[i], root)
		// One live circuit hint each keeps a 3-provider link below the 2048-byte bound.
		p := one.Providers[0]
		p.Addrs = nil
		for _, a := range one.Providers[0].Addrs {
			if host.IsCircuitAddress(a) {
				p.Addrs = append(p.Addrs, a)
				break
			}
		}
		combined.Providers = append(combined.Providers, p)
		labCheck(t, ns[i].NFT("add table inet magic\nadd counter inet magic sent\nadd chain inet magic output { type filter hook output priority 0; policy accept; }\nadd rule inet magic output udp sport 4171 counter name sent\n"))
	}
	if _, err = link.Parse(combined.String()); err != nil {
		t.Fatal(err)
	}
	magicDisk(t, work)
	start = time.Now()
	// Observe all three sending >1 MiB in the SAME progress interval. Protocol
	// handshakes/have messages are far smaller, so merely connecting cannot pass.
	var previous [3]uint64
	var sampleAt int64
	overlap := false
	finalOutput := filepath.Join(work, "three.bin")
	labCheck(t, magicGet(t, ns[3], bin, dirs[3], finalOutput, combined.String(), func(done int64) {
		if done-sampleAt < 64<<20 {
			return
		}
		sampleAt = done
		all := true
		for i := 0; i < 3; i++ {
			n := magicCounter(t, ns[i])
			if n-previous[i] < 1<<20 {
				all = false
			}
			previous[i] = n
		}
		overlap = overlap || all
	}))
	if !overlap {
		t.Fatalf("did not observe all 3 providers transferring concurrently: %v", previous)
	}
	if magicDigest(t, finalOutput, size) != digest {
		t.Fatal("3-peer SHA-256 mismatch")
	}
	t.Logf("SCENARIO 2: %d-byte 3-provider CLI download SHA-256 verified in %v; overlapping provider wire bytes=%v (two nocopy providers share verified fetched backing file)", size, time.Since(start), previous)
	// t.TempDir and process cleanups reclaim every source, outboard, store/output.
}
