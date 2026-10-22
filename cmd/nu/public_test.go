package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
)

// `nu public add|ls|rm` work daemonless (the mark is a pin that waits for a daemon with
// [public] configured). Share without a public seed is covered by daemon.TestPublicListener.
func TestPublicCLI(t *testing.T) {
	r := loopbackRepo(t, t.TempDir())
	file := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(file, []byte("hello"), 0o644)
	exec := func(json bool, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		if json {
			args = append([]string{"--json"}, args...)
		}
		code := run(append([]string{"--nu-path", r.Root()}, args...), &out, &errb)
		return code, out.String(), errb.String()
	}
	// --json add streams operation events; the done event carries the result.
	_, out, _ := exec(true, "add", file)
	var ar apiv0.AddResult
	for _, line := range strings.Split(out, "\n") {
		var ev apiv0.Event
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Operation != nil {
			json.Unmarshal(ev.Operation.Result, &ar)
		}
	}
	if ar.CID == "" {
		t.Fatalf("add: %s", out)
	}
	if code, out, errs := exec(false, "public", "add", ar.CID); code != 0 || !strings.Contains(out, "public "+ar.CID) || !strings.Contains(out, "public.listen") {
		t.Fatalf("public add: %d %q %q", code, out, errs)
	}
	if code, out, _ := exec(false, "public", "ls"); code != 0 || strings.TrimSpace(out) != ar.CID {
		t.Fatalf("public ls: %d %q", code, out)
	}
	if code, out, _ := exec(false, "pin", "ls"); code != 0 || !strings.Contains(out, "\tpublic\t") {
		t.Fatalf("pin ls must show the public source: %d %q", code, out)
	}
	if code, _, _ := exec(false, "public", "rm", ar.CID); code != 0 {
		t.Fatal("public rm")
	}
	if code, _, _ := exec(false, "public", "rm", ar.CID); code != apiv0.ExitNotFound {
		t.Fatalf("second rm: %d", code)
	}
}
