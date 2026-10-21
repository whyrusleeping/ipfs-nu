package tree_test

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/mst"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/nutest/slow"
)

type measuredStore struct {
	mst.MemStore
	reads, bytes int
}

func (s *measuredStore) Get(c cid.CID) ([]byte, error) {
	b, e := s.MemStore.Get(c)
	s.reads++
	s.bytes += len(b)
	return b, e
}

// One opt-in measurement, not a change to the production fanout/profile. It uses
// real directory name distributions and fixed-size entry values, not file I/O.
func TestHardeningFanout(t *testing.T) {
	slow.Skip(t)
	realRoot := os.Getenv("NU_MST_TREE")
	if realRoot == "" {
		root, err := exec.Command("go", "env", "GOROOT").Output()
		if err != nil {
			t.Fatal(err)
		}
		realRoot = filepath.Join(strings.TrimSpace(string(root)), "src")
	}
	dirs := map[string][]string{}
	err := filepath.WalkDir(realRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == realRoot {
			return nil
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}
		dirs[filepath.Dir(path)] = append(dirs[filepath.Dir(path)], d.Name())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var real [][]string
	paths := make([]string, 0, len(dirs))
	for p := range dirs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		sort.Strings(dirs[p])
		real = append(real, dirs[p])
	}
	modules := make([][]string, 1000)
	for i := range modules {
		for j := range 100 {
			modules[i] = append(modules[i], fmt.Sprintf("module-%04d-file-%04d.js", i, j))
		}
	}
	million := make([]string, 1_000_000)
	for i := range million {
		million[i] = fmt.Sprintf("entry-%07d.dat", i)
	}
	value := tree.Entry{Kind: tree.KindFile, CID: cid.New(cid.Raw, cid.Sum(cid.BLAKE3, []byte("value"))), Size: 5}
	for _, dataset := range []struct {
		name string
		dirs [][]string
	}{{"real", real}, {"node_modules_like", modules}, {"flat_1M", [][]string{million}}} {
		for _, bits := range []int{4, 5, 2} {
			t.Run(fmt.Sprintf("%s/fanout%d", dataset.name, 1<<bits), func(t *testing.T) {
				profile := *tree.DirProfile
				profile.LevelBits = bits
				st := &measuredStore{MemStore: mst.MemStore{}}
				roots := make([]cid.CID, len(dataset.dirs))
				start := time.Now()
				entries := 0
				for i, keys := range dataset.dirs {
					tr := profile.Load(st, cid.CID{})
					for _, key := range keys {
						if err := tr.Put(key, value); err != nil {
							t.Fatal(err)
						}
						entries++
					}
					var err error
					roots[i], err = tr.Flush()
					if err != nil {
						t.Fatal(err)
					}
				}
				build := time.Since(start)
				total, maxSize := 0, 0
				for _, b := range st.MemStore {
					total += len(b)
					maxSize = max(maxSize, len(b))
				}
				times := make([]time.Duration, 1000)
				st.reads = 0
				st.bytes = 0
				for i := range times {
					dir := i % len(dataset.dirs)
					keys := dataset.dirs[dir]
					key := keys[(i*7919)%len(keys)]
					// A fresh handle on each lookup excludes in-memory tree-node cache hits.
					start := time.Now()
					got, ok, err := profile.Load(st, roots[dir]).Get(key)
					times[i] = time.Since(start)
					if err != nil || !ok || got != value {
						t.Fatalf("lookup %q: %v", key, err)
					}
				}
				sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
				t.Logf("entries=%d dirs=%d build=%s blocks=%d encoded_bytes=%d max_node=%d cold_lookup_p50=%s p95=%s reads/op=%.2f bytes/op=%.0f real_source=%s", entries, len(roots), build, len(st.MemStore), total, maxSize, times[500], times[950], float64(st.reads)/1000, float64(st.bytes)/1000, realRoot)
			})
		}
	}
}
