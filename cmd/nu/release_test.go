package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
)

// `dataset gc` on a one-node mirror (0048): the dry run lists the previous head as a
// releasable version and changes nothing; gc unpins it; the old bytes are no longer
// pinned while the current head still reads; `member ls` outside a cluster is not_found.
func TestDatasetGCCLI(t *testing.T) {
	r := loopbackRepo(t, t.TempDir())
	src := t.TempDir()
	nu := func(args ...string) (int, string) {
		t.Helper()
		var out, errb bytes.Buffer
		code := run(append([]string{"--nu-path", r.Root()}, args...), &out, &errb)
		return code, out.String() + errb.String()
	}
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one"), 0o644)
	if code, out := nu("dataset", "create", "docs", "--source", src); code != 0 {
		t.Fatal(out)
	}
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("two"), 0o644)
	if code, out := nu("dataset", "update", "docs"); code != 0 {
		t.Fatal(out)
	}
	var rep apiv0.ReleaseReport
	code, out := nu("--json", "dataset", "gc", "docs", "--dry-run")
	if err := json.Unmarshal([]byte(out[:bytes.LastIndexByte([]byte(out), '}')+1]), &rep); code != 0 || err != nil {
		t.Fatalf("dry run: %d %s %v", code, out, err)
	}
	if !rep.DryRun || !rep.HeadConfirmed || len(rep.Candidates) != 1 || !rep.Candidates[0].OK || rep.Candidates[0].Kind != "version" || len(rep.Released) != 0 {
		t.Fatalf("dry run report: %+v", rep)
	}
	old := rep.Candidates[0].CID
	if code, out := nu("pin", "ls"); code != 0 || !bytes.Contains([]byte(out), []byte(old)) {
		t.Fatalf("dry run unpinned: %s", out)
	}
	if code, out := nu("dataset", "gc", "docs"); code != 0 || !bytes.Contains([]byte(out), []byte("released 1 version")) {
		t.Fatalf("gc: %d %s", code, out)
	}
	if code, out := nu("pin", "ls"); code != 0 || bytes.Contains([]byte(out), []byte(old)) {
		t.Fatalf("still pinned after gc: %s", out)
	}
	if code, out := nu("cat", "docs/a.txt"); code != 0 || !bytes.HasPrefix([]byte(out), []byte("two")) {
		t.Fatalf("current head: %d %q", code, out)
	}
	if code, _ := nu("cluster", "member", "ls"); code != apiv0.ExitNotFound {
		t.Fatalf("member ls outside a cluster: exit %d", code)
	}
}
