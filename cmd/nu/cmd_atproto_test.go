package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/nutest/atfix"
)

// `car import --offline` of a real PDS CAR verifies structure, pins the snapshot and makes
// it browsable with `ls at://`; `car export` writes streamable order that re-imports;
// a tree round-trips through nu-tree CARs; `get at://…` with -o is refused for repos.
func TestCarImportExport(t *testing.T) {
	root := loopbackRepo(t, filepath.Join(t.TempDir(), "nu")).Root()
	src := filepath.Join(atfix.VectorDir(), "ts-pds-rpozqnrysnmae4hegfralite.car")
	var out, errb bytes.Buffer
	run := func(args ...string) int {
		out.Reset()
		errb.Reset()
		return run(append([]string{"--nu-path", root}, args...), &out, &errb)
	}
	if code := run("car", "import", "--offline", src); code != apiv0.ExitOK {
		t.Fatalf("import: exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "atproto repository of "+atfix.DID) || !strings.Contains(out.String(), "signature unverified") || !strings.Contains(out.String(), "pinned ") {
		t.Fatalf("import output: %q", out.String())
	}
	commit := strings.Fields(strings.TrimPrefix(out.String(), "imported "))[0]
	if code := run("ls", "at://"+atfix.DID+"/app.bsky.feed.like"); code != apiv0.ExitOK || !strings.Contains(out.String(), "3lvwmxcdh2s27") {
		t.Fatalf("ls: exit %d: %q %s", code, out.String(), errb.String())
	}
	if code := run("cat", atfix.LikeURI); code != apiv0.ExitOK || !strings.Contains(out.String(), `"$type":"app.bsky.feed.like"`) {
		t.Fatalf("cat: exit %d: %q %s", code, out.String(), errb.String())
	}
	exp := filepath.Join(t.TempDir(), "alice.car")
	if code := run("car", "export", "-o", exp, commit); code != apiv0.ExitOK {
		t.Fatalf("export: exit %d: %s", code, errb.String())
	}
	exported, _ := os.ReadFile(exp)
	original, _ := os.ReadFile(src)
	if len(exported) == 0 || bytes.Equal(exported, original) {
		t.Fatalf("export is %d bytes (source %d; order should differ)", len(exported), len(original))
	}
	other := loopbackRepo(t, filepath.Join(t.TempDir(), "other")).Root()
	if code := run("--nu-path", other, "car", "import", "--offline", "--profile", "atrepo", exp); code != apiv0.ExitOK {
		t.Fatalf("re-import: exit %d: %s", code, errb.String())
	}
	if code := run("car", "import", "--profile", "nu-tree", exp); code != apiv0.ExitUsage || !strings.Contains(errb.String(), "not a Nu tree") {
		t.Fatalf("atrepo CAR as nu-tree: exit %d: %s", code, errb.String())
	}
	// A tree round-trips.
	site := filepath.Join(t.TempDir(), "site")
	os.MkdirAll(site, 0o755)
	os.WriteFile(filepath.Join(site, "a.txt"), []byte("hello"), 0o644)
	if code := run("add", "-r", site); code != apiv0.ExitOK {
		t.Fatal(errb.String())
	}
	treeCID := strings.Fields(out.String())[1]
	treeCar := filepath.Join(t.TempDir(), "site.car")
	if code := run("car", "export", "-o", treeCar, treeCID); code != apiv0.ExitOK {
		t.Fatalf("tree export: %s", errb.String())
	}
	if code := run("--nu-path", other, "car", "import", treeCar); code != apiv0.ExitOK || !strings.Contains(out.String(), "nu-tree profile") {
		t.Fatalf("tree import: exit %d: %q %s", code, out.String(), errb.String())
	}
	if code := run("--nu-path", other, "cat", treeCID+"/a.txt"); code != apiv0.ExitOK || out.String() != "hello" {
		t.Fatalf("cat imported tree: %q %s", out.String(), errb.String())
	}
	if code := run("get", "-o", filepath.Join(t.TempDir(), "x"), "at://"+atfix.DID); code != apiv0.ExitUsage {
		t.Fatalf("get -o repo: exit %d: %s", code, errb.String())
	}
}
