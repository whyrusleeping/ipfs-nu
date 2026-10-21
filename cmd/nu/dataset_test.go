package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node/daemon"
)

// The dataset head and wait CLI over the API: update reports metadata and content apart,
// a ready wait succeeds on held content at k=1, and a wait that cannot be satisfied in
// time exits 6 with the snapshot.
func TestDatasetWaitCLI(t *testing.T) {
	r := loopbackRepo(t, t.TempDir())
	d, err := daemon.Start(context.Background(), daemon.Options{Repo: r, APIListen: "127.0.0.1:0", GatewayListen: "127.0.0.1:0", Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	nu := func(args ...string) (int, []byte, string) {
		t.Helper()
		var out, errb bytes.Buffer
		code := run(append([]string{"--nu-path", r.Root()}, args...), &out, &errb)
		return code, out.Bytes(), errb.String()
	}
	file := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(file, []byte("hello archive"), 0o644)
	if code, _, e := nu("dataset", "create", "notes"); code != 0 {
		t.Fatal(e)
	}
	code, out, e := nu("--json", "add", file)
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	var ev struct {
		Operation struct {
			Result apiv0.AddResult `json:"result"`
		} `json:"operation"`
	}
	if code != 0 || json.Unmarshal(lines[len(lines)-1], &ev) != nil {
		t.Fatalf("add: %d %s %s", code, out, e)
	}
	add := ev.Operation.Result
	code, out, e = nu("dataset", "update", "notes", add.CID)
	if code != 0 || !strings.Contains(string(out), "metadata: acknowledged") || !strings.Contains(string(out), "content: ") {
		t.Fatalf("update: %d %s %s", code, out, e)
	}
	code, out, e = nu("--json", "dataset", "wait", "notes", add.CID, "--timeout", "5s")
	var res apiv0.WaitResult
	if code != 0 || json.Unmarshal(out, &res) != nil || !res.Satisfied || res.Required != 1 {
		t.Fatalf("wait: %d %s %s", code, out, e)
	}
	// A target no head names stays unmet until the timeout: exit 6.
	other := "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"
	code, out, _ = nu("dataset", "wait", "notes", other, "--timeout", "200ms")
	if code != apiv0.ExitTimeout || !strings.Contains(string(out), "NOT satisfied") || !strings.Contains(string(out), "target_not_observed") {
		t.Fatalf("timeout wait: %d %s", code, out)
	}
}
