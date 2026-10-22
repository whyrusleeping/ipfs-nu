package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/daemon"
	"github.com/whyrusleeping/nu/nutest/atfix"
)

// lockedBuffer is a bytes.Buffer safe for a writer goroutine and a polling reader.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The names CLI over the API (0045): `login --no-browser` prints the authorization URL
// and completes when the fixture "browser" authorizes; `publish` prints the write and
// says it is not readiness; `resolve` shows source and age; `get` by name binds the
// CID; `follow`/`follow ls` show a held mirror; `names`, `devices`, `unfollow` and
// `logout` round-trip.
func TestNamesCLI(t *testing.T) {
	f := atfix.New(t)
	cfg := f.Config()
	r := loopbackRepo(t, t.TempDir())
	d, err := daemon.Start(context.Background(), daemon.Options{Repo: r, APIListen: "127.0.0.1:0", GatewayListen: "127.0.0.1:0", Logger: slog.New(slog.DiscardHandler), Atproto: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	nu := func(args ...string) (int, string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		code := run(append([]string{"--nu-path", r.Root()}, args...), &out, &errb)
		return code, out.String(), errb.String()
	}

	// Login: the CLI blocks until the browser authorizes.
	var out, errb lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"--nu-path", r.Root(), "login", "--no-browser", atfix.PubHandle}, &out, &errb)
	}()
	var url string
	for i := 0; i < 200 && url == ""; i++ {
		for _, line := range strings.Split(errb.String(), "\n") {
			if strings.HasPrefix(line, "http://") {
				url = line
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if url == "" {
		t.Fatalf("no authorization URL: %q", errb.String())
	}
	resp, err := f.Browser().Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if code := <-done; code != 0 || !strings.Contains(out.String(), "logged in as "+atfix.PubHandle) {
		t.Fatalf("login: %d %q %q", code, out.String(), errb.String())
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello names"), 0o644)
	code, o, e := nu("--json", "add", "-r", dir)
	lines := strings.Split(strings.TrimSpace(o), "\n")
	var ev struct {
		Operation struct {
			Result apiv0.AddResult `json:"result"`
		} `json:"operation"`
	}
	if code != 0 || json.Unmarshal([]byte(lines[len(lines)-1]), &ev) != nil {
		t.Fatalf("add: %d %s %s", code, o, e)
	}
	root := ev.Operation.Result.CID
	code, o, e = nu("publish", "docs", root)
	if code != 0 || !strings.Contains(o, "published docs → "+root) || !strings.Contains(o, "not replication readiness") {
		t.Fatalf("publish: %d %s %s", code, o, e)
	}
	code, o, _ = nu("resolve", atfix.PubHandle+"/docs")
	if code != 0 || !strings.Contains(o, root) || !strings.Contains(o, "cache answer") {
		t.Fatalf("resolve: %d %s", code, o)
	}
	code, o, _ = nu("--json", "resolve", "--fresh", atfix.PubDID+"/docs")
	var res apiv0.NameResolution
	if code != 0 || json.Unmarshal([]byte(o), &res) != nil || res.Source != "pds" || res.Target != root {
		t.Fatalf("fresh resolve: %d %s", code, o)
	}
	outDir := t.TempDir()
	if code, _, e := nu("get", atfix.PubHandle+"/docs/a.txt", "-o", filepath.Join(outDir, "a.txt")); code != 0 {
		t.Fatalf("get by name: %d %s", code, e)
	}
	if b, _ := os.ReadFile(filepath.Join(outDir, "a.txt")); string(b) != "hello names" {
		t.Fatalf("got %q", b)
	}
	code, o, e = nu("follow", atfix.PubHandle+"/docs", "--dataset", "docs-mirror")
	if code != 0 || !strings.HasPrefix(o, "docs-mirror\tat://"+atfix.PubDID) {
		t.Fatalf("follow: %d %s %s", code, o, e)
	}
	var held bool
	for i := 0; i < 100 && !held; i++ {
		code, o, _ = nu("follow", "ls")
		held = code == 0 && strings.Contains(o, "\tfull\theld") && strings.Contains(o, "held:     "+root)
		time.Sleep(50 * time.Millisecond)
	}
	if !held {
		t.Fatalf("follow ls: %d %s", code, o)
	}
	if code, o, _ = nu("names"); code != 0 || !strings.Contains(o, "1 name(s)") || !strings.Contains(o, "docs\t"+root) {
		t.Fatalf("names: %d %s", code, o)
	}
	if code, o, _ = nu("node", "register", "--label", "lab", "--seed"); code != 0 || !strings.Contains(o, `as "lab" (device,seed)`) {
		t.Fatalf("register: %d %s", code, o)
	}
	if code, o, _ = nu("devices"); code != 0 || !strings.Contains(o, "lab\tdevice,seed (this node)") {
		t.Fatalf("devices: %d %s", code, o)
	}
	if code, o, _ = nu("unfollow", "docs-mirror"); code != 0 || !strings.Contains(o, "unfollowed") {
		t.Fatalf("unfollow: %d %s", code, o)
	}
	if code, o, _ = nu("unpublish", "docs"); code != 0 || !strings.Contains(o, "unpublished at://") {
		t.Fatalf("unpublish: %d %s", code, o)
	}
	if code, _, e := nu("resolve", "--fresh", atfix.PubHandle+"/docs"); code != apiv0.ExitNotFound || !strings.Contains(e, "proven absent") {
		t.Fatalf("resolve after unpublish: %d %s", code, e)
	}
	if code, o, _ = nu("logout"); code != 0 || !strings.Contains(o, "logged out") {
		t.Fatalf("logout: %d %s", code, o)
	}
	if code, _, e := nu("publish", "docs", root); code != apiv0.ExitForbidden || !strings.Contains(e, "nu login") {
		t.Fatalf("publish after logout: %d %s", code, e)
	}
}
