package bridge

import (
	"context"
	"fmt"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/store"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const kuboImage = "ipfs/kubo:v0.40.0@sha256:e08c602bf97f138a0ec5b42155b4fd4532852919250b11b4aadbea2bf42c0e10"

func TestKuboCARImports(t *testing.T) {
	if os.Getenv("NU_DOCKER") != "1" {
		t.Skip("set NU_DOCKER=1 for opt-in pinned Kubo fixture generation")
	}
	dir := t.TempDir()
	cmd := exec.Command("docker", "run", "--rm", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--entrypoint", "/bin/sh", "-v", dir+":/fixture", "-w", "/fixture", kuboImage, "-ec", `ipfs init --profile=test >/dev/null
ipfs config Import.UnixFSHAMTDirectorySizeThreshold 1
mkdir -p files/sub
printf 'small-data\n' > files/hello.txt
printf '#!/bin/sh\n' > files/executable
chmod 755 files/executable
head -c 3145728 /dev/urandom > files/sub/large.bin
for i in $(seq -w 0 1200); do printf 'fixture-%s\n' "$i" > "files/f$i"; done
ln -s 'hello.txt' files/link
ipfs add -Q -r --cid-version=1 --raw-leaves=true files > root-v1
ipfs dag export "$(cat root-v1)" > root-v1.car
ipfs get "$(cat root-v1)" -o get-v1 >/dev/null
ipfs add -Q -r --preserve-mode --cid-version=0 files > root-v0
ipfs dag export "$(cat root-v0)" > root-v0.car
ipfs get "$(cat root-v0)" -o get-v0 >/dev/null
`)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("Kubo fixture generation failed: %v\n%s", e, out)
	}
	repo, err := store.Open(filepath.Join(t.TempDir(), "nu"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	for _, ver := range []string{"v1", "v0"} {
		t.Run(ver, func(t *testing.T) {
			carPath := filepath.Join(dir, "root-"+ver+".car")
			f, e := os.Open(carPath)
			if e != nil {
				t.Fatal(e)
			}
			src, e := NewCARSource(f)
			f.Close()
			if e != nil {
				t.Fatal(e)
			}
			defer src.Close()
			rootText, e := os.ReadFile(filepath.Join(dir, "root-"+ver))
			if e != nil {
				t.Fatal(e)
			}
			root, e := ParseIPFSCID(strings.TrimSpace(string(rootText)))
			if e != nil {
				t.Fatal(e)
			}
			if ver == "v0" && root.Version != 0 {
				t.Fatalf("expected CIDv0 root, got %s", root.String())
			}
			if ver == "v1" {
				rootBlock, e := src.GetBlock(context.Background(), root)
				if e != nil {
					t.Fatal(e)
				}
				pb, e := DecodePBNode(rootBlock)
				if e != nil {
					t.Fatal(e)
				}
				unix, e := DecodeUnixFSData(pb.Data)
				if e != nil {
					t.Fatal(e)
				}
				if unix.Type != UnixFSHAMTShard {
					t.Fatalf("Kubo fixture did not produce HAMTShard: type %d", unix.Type)
				}
				rawLeaves := 0
				for block := range src.blocks {
					if block.Codec == codecRaw {
						rawLeaves++
					}
				}
				if rawLeaves == 0 {
					t.Fatal("Kubo fixture did not produce raw leaves")
				}
			}
			res, e := Import(context.Background(), root, src, repo, repo, ImportOptions{Source: "kubo-car", ImporterVersion: "test"})
			if e != nil {
				t.Fatal(e)
			}
			raw, e := repo.Get(res.Root)
			if e != nil {
				t.Fatal(e)
			}
			h, e := tree.DecodeHeader(raw)
			if e != nil {
				t.Fatal(e)
			}
			sourceRoot := filepath.Join(dir, "get-"+ver, "files")
			if _, e = os.Stat(sourceRoot); e != nil {
				sourceRoot = filepath.Join(dir, "get-"+ver)
			}
			compareTree(t, repo, h.Root, sourceRoot)
		})
	}
}
func compareTree(t *testing.T, st *store.Store, root cid.CID, dir string) {
	t.Helper()
	if err := tree.Dir(st, root).Walk("", func(name string, e tree.Entry) (bool, error) {
		path := filepath.Join(dir, name)
		switch e.Kind {
		case tree.KindDir:
			if err := compareTreeErr(t, st, e.CID, path); err != nil {
				return false, err
			}
		case tree.KindFile:
			b, err := readBlob(st, e.CID)
			if err != nil {
				return false, err
			}
			want, err := os.ReadFile(path)
			if err != nil {
				return false, err
			}
			if !bytesEqual(b, want) {
				return false, fmt.Errorf("bytes differ at %s", path)
			}
			info, err := os.Stat(path)
			if err != nil {
				return false, err
			}
			if (e.Mode == tree.ModeExec) != (info.Mode().Perm()&0o100 != 0) {
				return false, fmt.Errorf("executable mode differs at %s: imported=%o extracted=%o", path, e.Mode, info.Mode().Perm())
			}
		case tree.KindSymlink:
			target, err := os.Readlink(path)
			if err != nil {
				return false, err
			}
			b, err := readBlob(st, e.CID)
			if err != nil {
				return false, err
			}
			if string(b) != target {
				return false, fmt.Errorf("symlink differs at %s", path)
			}
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
}
func compareTreeErr(t *testing.T, st *store.Store, root cid.CID, dir string) error {
	t.Helper()
	return tree.Dir(st, root).Walk("", func(name string, e tree.Entry) (bool, error) {
		path := filepath.Join(dir, name)
		switch e.Kind {
		case tree.KindDir:
			return true, compareTreeErr(t, st, e.CID, path)
		case tree.KindFile:
			b, err := readBlob(st, e.CID)
			if err != nil {
				return false, err
			}
			w, err := os.ReadFile(path)
			if err != nil {
				return false, err
			}
			if !bytesEqual(b, w) {
				return false, fmt.Errorf("bytes differ at %s", path)
			}
		case tree.KindSymlink:
			target, err := os.Readlink(path)
			if err != nil {
				return false, err
			}
			b, err := readBlob(st, e.CID)
			if err != nil {
				return false, err
			}
			if string(b) != target {
				return false, fmt.Errorf("symlink differs at %s", path)
			}
		}
		return true, nil
	})
}
func readBlob(st *store.Store, c cid.CID) ([]byte, error) {
	b, e := st.OpenBlob(c.Multihash())
	if e != nil {
		return nil, e
	}
	defer b.Close()
	return io.ReadAll(b.Reader())
}
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
