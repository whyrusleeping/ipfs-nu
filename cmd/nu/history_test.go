package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
)

// The individual's archive from the CLI, daemonless (0047): an explicit one-copy archive
// preset with a source directory, a second version by re-adding it, exact old bytes by
// TID and label, and rollback as a new head.
func TestCLIArchive(t *testing.T) {
	r := loopbackRepo(t, t.TempDir())
	src := t.TempDir()
	nu := func(args ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		if code := run(append([]string{"--nu-path", r.Root()}, args...), &out, &errb); code != apiv0.ExitOK {
			t.Fatalf("nu %v: exit %d: %s", args, code, errb.String())
		}
		return out.String()
	}
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one"), 0o644)
	var d apiv0.Dataset
	json.Unmarshal([]byte(nu("--json", "dataset", "create", "photos", "--archive", "--source", src)), &d)
	if d.Version == nil || d.Replicas != 1 || d.History == nil {
		t.Fatalf("create: %+v", d)
	}
	v1 := d.Version.TID
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("two"), 0o644)
	nu("dataset", "update", "photos")
	var vs apiv0.DatasetVersions
	json.Unmarshal([]byte(nu("--json", "dataset", "versions", "photos")), &vs)
	if len(vs.Versions) != 2 || vs.Versions[0].TID != v1 || vs.Archive == nil || vs.Archive.State != "ready" {
		t.Fatalf("versions: %+v", vs)
	}
	if got := nu("cat", "photos@"+v1+"/a.txt"); got != "one" {
		t.Fatalf("old bytes %q", got)
	}
	nu("dataset", "label", "photos", v1, "first")
	nu("dataset", "rollback", "photos", "first")
	if got := nu("cat", "photos/a.txt"); got != "one" {
		t.Fatalf("after rollback %q", got)
	}
	out := filepath.Join(t.TempDir(), "restore")
	nu("get", "photos@"+vs.Versions[1].TID, "-o", out)
	if b, _ := os.ReadFile(filepath.Join(out, "a.txt")); string(b) != "two" {
		t.Fatalf("restored %q", b)
	}
}
