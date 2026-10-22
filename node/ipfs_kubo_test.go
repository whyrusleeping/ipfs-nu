package node

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mr-tron/base58/base58"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/kad"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/p2p/wire"
)

const kuboImage = "ipfs/kubo:v0.40.0@sha256:e08c602bf97f138a0ec5b42155b4fd4532852919250b11b4aadbea2bf42c0e10"

func kubo(t *testing.T, dir string, detach bool, script string) string {
	t.Helper()
	args := []string{"run", "--rm", "--network", "host", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--env", "IPFS_PATH=/fixture/repo",
		"--entrypoint", "/bin/sh", "-v", dir + ":/fixture", "-w", "/fixture"}
	if detach {
		args = append(args, "-d")
	}
	out, err := exec.Command("docker", append(args, kuboImage, "-ec", script)...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubo: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// Pinset migration from a real Kubo v0.40.0 repo (NU_DOCKER=1): the operator's read-only
// `ipfs pin ls` inventory and `ipfs dag export` CARs feed a dry run and an import with a
// per-root reconciliation (one CAR deliberately absent → missing); a second Nu node imports
// the same pinset over Bitswap/QUIC-v1 from the Kubo daemon and gets identical Nu roots.
// The Kubo repo's files are byte-identical after the CAR import, and its pins and block set
// are unchanged after the network import.
func TestKuboPinsetMigration(t *testing.T) {
	if os.Getenv("NU_DOCKER") != "1" {
		t.Skip("set NU_DOCKER=1 for the opt-in Kubo pinset migration test")
	}
	dir := t.TempDir()
	kubo(t, dir, false, `ipfs init --profile=test >/dev/null
ipfs config Addresses.Swarm --json '["/ip4/127.0.0.1/udp/44173/quic-v1"]'
ipfs config Bootstrap --json '[]'
mkdir -p site/sub
printf 'hello\n' > site/index.html
head -c 600000 /dev/urandom > site/sub/blob.bin
chmod 755 site/index.html
ln -s index.html site/home
head -c 300000 /dev/urandom > single.bin
ipfs add -Q -r --cid-version=1 --raw-leaves site > site-cid
ipfs add -Q --pin-name=single single.bin > single-cid
printf 'gone\n' > gone.txt
ipfs add -Q --cid-version=1 gone.txt > gone-cid
ipfs pin ls --type recursive --enc=json --names > pins.json
mkdir cars
for c in $(ipfs pin ls --type recursive --quiet); do ipfs dag export "$c" > "cars/$c.car"; done
rm "cars/$(cat gone-cid).car"
{ ipfs pin ls --enc=json; ipfs refs local | sort; } > logical-before
ipfs id -f='<id>' > peer
(cd repo && find . -type f | sort | xargs sha256sum) > repo-before.sum
`)
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	site, single, gone := read("site-cid"), read("single-cid"), read("gone-cid")

	open := func() *Local {
		r, _, err := repo.Open(filepath.Join(t.TempDir(), "nu"))
		if err != nil {
			t.Fatal(err)
		}
		n, err := NewLocal(r)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { n.Close() })
		return n
	}
	pins, cars := filepath.Join(dir, "pins.json"), filepath.Join(dir, "cars")
	n := open()
	dry := runPinset(t, n, apiv0.IPFSImportRequest{Pins: pins, CARDir: cars, DryRun: true})
	statuses := func(res apiv0.IPFSPinsetResult) map[string]apiv0.IPFSRootReport {
		m := map[string]apiv0.IPFSRootReport{}
		for _, r := range res.Roots {
			m[r.IPFSRoot] = r
		}
		return m
	}
	ds := statuses(dry)
	if ds[site].Status != apiv0.IPFSPlanned || ds[single].Status != apiv0.IPFSPlanned || ds[single].Name != "single" || ds[gone].Status != apiv0.IPFSMissing {
		t.Fatalf("dry run: %+v", dry)
	}
	res := runPinset(t, n, apiv0.IPFSImportRequest{Pins: pins, CARDir: cars})
	cs := statuses(res)
	if cs[site].Status != apiv0.IPFSImported || cs[single].Status != apiv0.IPFSImported || cs[gone].Status != apiv0.IPFSMissing || cs[site].Root != ds[site].Root {
		t.Fatalf("CAR pinset import: %+v", res)
	}
	t.Logf("CAR pinset: %+v", res.Counts)
	rc, err := n.Cat(context.Background(), cs[site].Root+"/sub/blob.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if want, _ := os.ReadFile(filepath.Join(dir, "site/sub/blob.bin")); !bytes.Equal(got, want) {
		t.Fatal("imported bytes differ from the Kubo source file")
	}

	// The Kubo repo is untouched by the CAR import; then the same pinset over Bitswap.
	id := kubo(t, dir, true, `(cd repo && find . -type f | sort | xargs sha256sum) > repo-after.sum
ipfs daemon > daemon.log 2>&1 &
pid=$!
for i in $(seq 1 300); do if grep -q 'Daemon is ready' daemon.log; then touch ready; break; fi; sleep 0.1; done
wait "$pid"
`)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", id).CombinedOutput()
			t.Fatalf("Kubo daemon not ready: %s", logs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if read("repo-after.sum") != read("repo-before.sum") {
		t.Fatal("the Kubo repo changed during the CAR pinset import")
	}
	pb, err := base58.Decode(read("peer"))
	if err != nil {
		t.Fatal(err)
	}
	peer, err := wire.PeerIDFromBytes(pb)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := wire.ParseMultiaddr("/ip4/127.0.0.1/udp/44173/quic-v1/p2p/"+peer.String(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	m := open()
	if err := m.AttachNetwork(fakeNet{h: testHost(t), p: kad.Peer{ID: peer, Addrs: [][]byte{addr.Bytes()}}}); err != nil {
		t.Fatal(err)
	}
	net := runPinset(t, m, apiv0.IPFSImportRequest{Pins: pins})
	ns := statuses(net)
	for _, c := range []string{site, single} {
		if ns[c].Status != apiv0.IPFSImported || ns[c].Root != cs[c].Root {
			t.Fatalf("network import of %s: %+v (CAR: %+v)", c, ns[c], cs[c])
		}
	}
	t.Logf("network pinset: %+v", net.Counts)
	if out, err := exec.Command("docker", "exec", id, "ipfs", "shutdown").CombinedOutput(); err != nil {
		t.Fatalf("ipfs shutdown: %v %s", err, out)
	}
	_ = exec.Command("docker", "wait", id).Run()
	kubo(t, dir, false, `{ ipfs pin ls --enc=json; ipfs refs local | sort; } > logical-after`)
	if read("logical-after") != read("logical-before") {
		t.Fatal("Kubo pins or blocks changed during the network pinset import")
	}
}
