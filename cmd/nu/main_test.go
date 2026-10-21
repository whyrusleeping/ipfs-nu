package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/daemon"
	"github.com/whyrusleeping/nu/node/repo"
)

// The CLI follows an operation through the client and exits with the error category.
func TestCLIFollowsOperationAndExitCodes(t *testing.T) {
	r, _, err := repo.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := daemon.Start(context.Background(), daemon.Options{Repo: r, APIListen: "127.0.0.1:0", GatewayListen: "127.0.0.1:0", Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	o := d.Node.Ops.Start("test", "bafytarget")
	o.Stage("working")
	o.Progress(1, 2, "items")
	o.Succeed(map[string]string{"cid": "bafydone"})

	var out, errb bytes.Buffer
	code := run([]string{"--nu-path", r.Root(), "--json", "operations", "wait", o.ID()}, &out, &errb)
	if code != apiv0.ExitOK {
		t.Fatalf("exit %d, stderr %s", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var last apiv0.Event
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || last.Type != apiv0.EventDone || last.Operation.State != apiv0.StateSucceeded {
		t.Fatalf("events: %s", out.String())
	}

	// A failed operation exits with its error's category; a not-yet-supported target with
	// not_supported; no daemon with unavailable. Errors carry an action.
	f := d.Node.Ops.Start("test", "x")
	f.Fail(apiv0.Errorf(apiv0.CodeNotFound, "gone").WithAction("look elsewhere"))
	cases := []struct {
		args []string
		exit int
	}{
		{[]string{"--nu-path", r.Root(), "operations", "wait", f.ID()}, apiv0.ExitNotFound},
		{[]string{"--nu-path", r.Root(), "cat", "alice.example.com/site"}, apiv0.ExitNotSupported},
		{[]string{"--nu-path", r.Root(), "cat", "not a target"}, apiv0.ExitUsage},
		{[]string{"--nu-path", r.Root(), "cat", "bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi"}, apiv0.ExitUsage}, // dag-pb: not a DASL CID
		{[]string{"--nu-path", r.Root(), "get", "bafkreiccldh766hwcnuxnf2wh6jgzepf2nlu2lvcllt63eww5p6chi4ity"}, apiv0.ExitNotImplemented},
		{[]string{"--nu-path", t.TempDir(), "--json", "operations"}, apiv0.ExitUnavailable},
		{[]string{"--nu-path", r.Root(), "bogus"}, apiv0.ExitUsage},
	}
	// Daemonless: cat/ls/add run an in-process node when no daemon runs; a held lock
	// without an answering API is a clear unavailable error.
	lonely, _, _ := repo.Open(t.TempDir())
	cid := "bafkreiccldh766hwcnuxnf2wh6jgzepf2nlu2lvcllt63eww5p6chi4ity"
	cases = append(cases,
		struct {
			args []string
			exit int
		}{[]string{"--nu-path", lonely.Root(), "cat", cid}, apiv0.ExitNotFound},
		struct {
			args []string
			exit int
		}{[]string{"--nu-path", lonely.Root(), "add", filepath.Join(t.TempDir(), "missing")}, apiv0.ExitNotFound},
	)
	locked, _, _ := repo.Open(t.TempDir())
	lk, err := locked.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	cases = append(cases, struct {
		args []string
		exit int
	}{[]string{"--nu-path", locked.Root(), "ls", cid}, apiv0.ExitUnavailable})

	for _, tc := range cases {
		out.Reset()
		errb.Reset()
		if code := run(tc.args, &out, &errb); code != tc.exit {
			t.Errorf("%v: exit %d, want %d (stderr %q)", tc.args, code, tc.exit, errb.String())
		}
		if tc.exit != apiv0.ExitUsage && !strings.Contains(errb.String(), "→") && !strings.Contains(errb.String(), `"action"`) {
			t.Errorf("%v: error without action: %q", tc.args, errb.String())
		}
	}
	if _, err := lonely.ReadAddrFile(repo.APIFile); err != nil {
		t.Fatal(err)
	}
	if l, err := lonely.Lock(); err != nil {
		t.Fatalf("in-process node did not release the lock: %v", err)
	} else {
		l.Release()
	}
}

// From a fresh $NU_PATH with no init: `add -r` of a site without a daemon, then the site
// renders on its <cid>.localhost origin and lists on /tree/<cid>/ once a daemon runs;
// `--json` output and daemonless pin/ls follow suit.
func TestAddSiteAndBrowse(t *testing.T) {
	site := filepath.Join(t.TempDir(), "site")
	os.MkdirAll(filepath.Join(site, "sub"), 0o755)
	os.WriteFile(filepath.Join(site, "index.html"), []byte("<h1>hello</h1>"), 0o644)
	os.WriteFile(filepath.Join(site, "sub", "style.css"), []byte("body{}"), 0o644)
	root := filepath.Join(t.TempDir(), "nu")

	var out, errb bytes.Buffer
	if code := run([]string{"--nu-path", root, "add", "-r", site}, &out, &errb); code != apiv0.ExitOK {
		t.Fatalf("add: exit %d: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "shared when a node runs") {
		t.Fatalf("no daemonless note: %q", errb.String())
	}
	fields := strings.Fields(out.String())
	if len(fields) != 3 || fields[0] != "added" || fields[2] != "site" {
		t.Fatalf("add output: %q", out.String())
	}
	cid := fields[1]

	// --json streams the events and ends with the done event carrying the result.
	out.Reset()
	errb.Reset()
	if code := run([]string{"--nu-path", root, "--json", "add", "-r", site}, &out, &errb); code != apiv0.ExitOK {
		t.Fatalf("add --json: exit %d: %s", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var last apiv0.Event
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || last.Type != apiv0.EventDone ||
		!strings.Contains(string(last.Operation.Result), `"cid":"`+cid+`"`) {
		t.Fatalf("json add events: %s", out.String())
	}
	out.Reset()
	if code := run([]string{"--nu-path", root, "--json", "ls", cid}, &out, &errb); code != apiv0.ExitOK {
		t.Fatalf("ls: exit %d: %s", code, errb.String())
	}
	var l apiv0.Listing
	if err := json.Unmarshal(out.Bytes(), &l); err != nil || len(l.Entries) != 2 || l.Entries[1].Name != "sub" {
		t.Fatalf("ls --json: %s %v", out.String(), err)
	}
	out.Reset()
	if code := run([]string{"--nu-path", root, "pin", "ls"}, &out, &errb); code != apiv0.ExitOK || !strings.Contains(out.String(), cid+"\trecursive\texplicit") {
		t.Fatalf("pin ls: exit %d: %s %s", code, out.String(), errb.String())
	}

	r, created, err := repo.Open(root)
	if err != nil || created {
		t.Fatal("repo was not initialized by add")
	}
	d, err := daemon.Start(context.Background(), daemon.Options{Repo: r, APIListen: "127.0.0.1:0", GatewayListen: "127.0.0.1:0", Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, port, _ := strings.Cut(d.GatewayAddr(), ":")
	get := func(host, path string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", "http://"+d.GatewayAddr()+path, nil)
		req.Host = host + ":" + port
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}
	res, body := get(cid+".localhost", "/")
	if res.StatusCode != 200 || body != "<h1>hello</h1>" || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("site index: %s %q %s", res.Status, body, res.Header.Get("Content-Type"))
	}
	res, body = get(cid+".localhost", "/sub/style.css")
	if res.StatusCode != 200 || body != "body{}" || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/css") {
		t.Fatalf("site css: %s %q %s", res.Status, body, res.Header.Get("Content-Type"))
	}
	res, body = get("localhost", "/tree/"+cid+"/")
	if res.StatusCode != 200 || !strings.Contains(body, `href="sub/"`) || !strings.Contains(body, `href="index.html"`) {
		t.Fatalf("listing: %s %q", res.Status, body)
	}
	res, _ = get("localhost", "/tree/"+cid+"/nope")
	if res.StatusCode != 404 {
		t.Fatalf("missing path: %s", res.Status)
	}
	// With the daemon running, status reports the store and the CLI goes through it.
	out.Reset()
	if code := run([]string{"--nu-path", root, "--json", "status"}, &out, &errb); code != apiv0.ExitOK || !strings.Contains(out.String(), `"pins": 1`) {
		t.Fatalf("status: exit %d: %s", code, out.String())
	}
}
