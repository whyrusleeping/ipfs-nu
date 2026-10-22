package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/client"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/nutest/timeouts"
)

// End to end: a fresh NU_PATH is initialized on first use, the daemon serves API and
// gateway on ephemeral ports, the client finds it through the repo, follows an operation
// via events and wait, and errors arrive as the typed envelope.
func TestDaemonEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.Ceiling())
	defer cancel()

	root := filepath.Join(t.TempDir(), "nu")
	r, created, err := repo.Open(root)
	if err != nil || !created {
		t.Fatalf("auto-init: created=%v err=%v", created, err)
	}
	if fi, err := os.Stat(root); err != nil || fi.Mode().Perm() != repo.DirMode {
		t.Fatalf("repo dir mode %v err %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(r.Path(repo.ConfigFile)); err != nil || fi.Mode().Perm() != repo.FileMode {
		t.Fatalf("config mode %v err %v", fi.Mode(), err)
	}
	if err := r.ConfigSet("gc.release_delay", "30"); err != nil {
		t.Fatal(err)
	}
	if v, err := r.ConfigGet("gc.release_delay"); err != nil || v != int64(30) {
		t.Fatalf("config get: %v %v", v, err)
	}
	if cfg, err := r.Config(); err != nil || cfg.API.Listen != repo.DefaultAPIListen {
		t.Fatalf("typed config after set: %+v %v", cfg, err)
	}

	// Before the daemon runs, discovery says so with an action.
	if _, err := client.Discover(r); !isCode(err, apiv0.CodeUnavailable) {
		t.Fatalf("discover without daemon: %v", err)
	}

	d, err := Start(ctx, Options{Repo: r, APIListen: "127.0.0.1:0", GatewayListen: "127.0.0.1:0",
		SwarmListen: []string{"/ip4/127.0.0.1/udp/0/quic-v1"}, NoMDNS: true, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Non-loopback listeners are refused until authentication exists (operations.md A1).
	for _, o := range []Options{{Repo: r, APIListen: "0.0.0.0:0"}, {Repo: r, GatewayListen: "[::]:0"}} {
		if _, err := Start(ctx, o); !isCode(err, apiv0.CodeInvalidArgument) {
			t.Fatalf("non-loopback listener %+v: %v", o, err)
		}
	}

	// A second daemon on the same repo is refused.
	if _, err := Start(ctx, Options{Repo: r, APIListen: "127.0.0.1:0", GatewayListen: "127.0.0.1:0"}); !isCode(err, apiv0.CodeConflict) {
		t.Fatalf("second daemon: %v", err)
	}

	c, err := client.Discover(r)
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Daemon.API != d.APIAddr() || st.Paths.Root != root || st.Daemon.PID != os.Getpid() {
		t.Fatalf("status: %+v", st)
	}

	// Gateway placeholder answers.
	res, err := http.Get("http://" + d.GatewayAddr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("gateway root: %s", res.Status)
	}

	// An operation: started in-process (as add/fetch will), followed through the client.
	o := d.Node.Ops.Start("test", "bafytarget")
	stream, err := c.OperationEvents(ctx, o.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	o.Stage("hashing")
	o.Progress(5, 10, "bytes")
	var seen []apiv0.EventType
	for len(seen) < 2 {
		ev, err := stream.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, ev.Type)
	}
	if seen[0] != apiv0.EventStage || seen[1] != apiv0.EventProgress {
		t.Fatalf("events: %v", seen)
	}
	// Wait times out while running, with the snapshot in details, and the op ID matches.
	if _, err := c.WaitOperation(ctx, o.ID(), 10*time.Millisecond); !isCode(err, apiv0.CodeTimeout) {
		t.Fatalf("wait while running: %v", err)
	}
	go o.Succeed(apiv0.AddResult{CID: "bafydone", Size: 10})
	final, err := c.WaitOperation(ctx, o.ID(), 0)
	if err != nil || final.State != apiv0.StateSucceeded || len(final.Result) == 0 {
		t.Fatalf("wait: %+v %v", final, err)
	}
	// The stream ends with the done event, then EOF.
	var last *apiv0.Event
	for {
		ev, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		last = ev
	}
	if last == nil || last.Type != apiv0.EventDone || last.Operation == nil || last.Operation.State != apiv0.StateSucceeded {
		t.Fatalf("last event: %+v", last)
	}
	// Listing pages with a cursor; a cursor from another epoch asks for a resync.
	page, err := c.Operations(ctx, "", 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != o.ID() {
		t.Fatalf("list: %+v %v", page, err)
	}
	page, err = c.Operations(ctx, apiv0.Cursor{Epoch: "old", Offset: 0}.Encode(), 10)
	if err != nil || !page.Resync {
		t.Fatalf("expired cursor: %+v %v", page, err)
	}

	// Error envelope: unknown operation, unsupported DID method (names resolve since
	// 0045; this form needs no network), relative add path.
	if _, err := c.Operation(ctx, "nope"); !isCode(err, apiv0.CodeNotFound) {
		t.Fatalf("unknown op: %v", err)
	}
	if _, err := c.Cat(ctx, "did:foo:bar/site"); !isCode(err, apiv0.CodeNotSupported) {
		t.Fatalf("name target: %v", err)
	}
	if _, err := c.Add(ctx, apiv0.AddRequest{Path: "x"}); !isCode(err, apiv0.CodeInvalidArgument) {
		t.Fatalf("add: %v", err)
	}
	// An add through the API: the operation runs past the request and cat reads it back.
	file := filepath.Join(t.TempDir(), "hello.txt")
	os.WriteFile(file, []byte("hello"), 0o644)
	ao, err := c.Add(ctx, apiv0.AddRequest{Path: file, Pin: true})
	if err != nil {
		t.Fatal(err)
	}
	fin, err := c.WaitOperation(ctx, ao.ID, 0)
	if err != nil || fin.State != apiv0.StateSucceeded {
		t.Fatalf("add wait: %+v %v", fin, err)
	}
	var ar apiv0.AddResult
	json.Unmarshal(fin.Result, &ar)
	rc, err := c.Cat(ctx, ar.CID)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(rc); string(b) != "hello" {
		t.Fatalf("cat via api: %q", b)
	}
	rc.Close()
	if st, err := c.Status(ctx); err != nil || st.Store == nil || st.Store.Pins != 1 {
		t.Fatalf("status store: %+v %v", st, err)
	}

	// Clean shutdown removes the address files and releases the lock.
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if addr, _ := r.ReadAddrFile(repo.APIFile); addr != "" {
		t.Fatalf("api file still present: %q", addr)
	}
	if _, err := client.Discover(r); !isCode(err, apiv0.CodeUnavailable) {
		t.Fatalf("discover after stop: %v", err)
	}
}

func isCode(err error, code apiv0.Code) bool {
	var ae *apiv0.Error
	return errors.As(err, &ae) && ae.Code == code && ae.Action != ""
}
