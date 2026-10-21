package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
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
		{[]string{"--nu-path", r.Root(), "pin", "ls"}, apiv0.ExitNotImplemented},
		{[]string{"--nu-path", t.TempDir(), "--json", "operations"}, apiv0.ExitUnavailable},
		{[]string{"--nu-path", r.Root(), "bogus"}, apiv0.ExitUsage},
	}
	// Daemonless: cat/ls/get run an in-process node when no daemon runs (the store is
	// absent, so they reach not_implemented); a held lock without an answering API is a
	// clear unavailable error.
	lonely, _, _ := repo.Open(t.TempDir())
	cid := "bafkr4iaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cases = append(cases,
		struct {
			args []string
			exit int
		}{[]string{"--nu-path", lonely.Root(), "cat", cid}, apiv0.ExitNotImplemented},
		struct {
			args []string
			exit int
		}{[]string{"--nu-path", lonely.Root(), "add", "main.go"}, apiv0.ExitUnavailable},
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
