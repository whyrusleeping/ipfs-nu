package node

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/cluster"
	"github.com/whyrusleeping/nu/gateway"
	"github.com/whyrusleeping/nu/node/repo"
	"github.com/whyrusleeping/nu/store"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if body == "" {
			os.Remove(filepath.Join(dir, name))
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// importDir re-adds dir (unpinned: the dataset's pins hold it) and writes the head.
func importDir(t *testing.T, n *Local, ds, dir string) *apiv0.Dataset {
	t.Helper()
	res, err := add(t, n, apiv0.AddRequest{Path: dir, Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	d, err := n.DatasetUpdate(context.Background(), ScopeLocal, ds, apiv0.DatasetUpdateRequest{Target: res.CID})
	if err != nil {
		t.Fatal(err)
	}
	if d.Version == nil || d.Version.Root != res.CID {
		t.Fatalf("head write appended no version: %+v", d.Version)
	}
	return d
}

func catString(t *testing.T, n *Local, target string) (string, error) {
	t.Helper()
	rc, err := n.Cat(context.Background(), target)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return string(b), err
}

func mustCat(t *testing.T, n *Local, target, want string) {
	t.Helper()
	got, err := catString(t, n, target)
	if err != nil || got != want {
		t.Fatalf("cat %s = %q, %v; want %q", target, got, err, want)
	}
}

func avail(t *testing.T, n *Local, ds string) map[string]apiv0.VersionInfo {
	t.Helper()
	vs, err := n.DatasetVersions(context.Background(), ScopeLocal, ds)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]apiv0.VersionInfo{}
	for _, v := range vs.Versions {
		out[v.TID] = v
	}
	return out
}

// The individual one-copy archive (0047): an explicit --archive preset on the local
// cluster, two imported versions, exact old bytes by TID/label/time (cat, get, gateway),
// diff, rollback as a new head, R9 one-node readiness (required 1) and archive-wide
// readiness, without a PDS or other nodes; history survives a restart.
func TestOneNodeArchive(t *testing.T) {
	ctx := context.Background()
	n := newLocal(t)
	src := t.TempDir()
	d, err := n.DatasetCreate(ctx, ScopeLocal, apiv0.DatasetCreateRequest{Name: "photos", Archive: true, SourcePath: src})
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 1 || d.History == nil || d.History.KeepLast != 10 || d.History.Yearly != -1 || d.SourcePath != src {
		t.Fatalf("archive preset: %+v", d)
	}
	writeFiles(t, src, map[string]string{"a.txt": "version one", "shared.txt": "shared bytes"})
	d1 := importDir(t, n, "photos", src)
	v1 := d1.Version
	if v1.Files != 2 || v1.NewBytes != int64(len("version one")+len("shared bytes")) || v1.Availability != apiv0.AvailComplete {
		t.Fatalf("v1 = %+v", v1)
	}
	writeFiles(t, src, map[string]string{"a.txt": "version two", "b.txt": "new file"})
	d2 := importDir(t, n, "photos", src)
	v2 := d2.Version
	if v2.Parent != v1.TID || v2.NewBytes != int64(len("version two")+len("new file")) {
		t.Fatalf("v2 = %+v", v2)
	}
	// R9: ready at k=1 with required 1; archive-wide readiness over the live set.
	if c := d2.Content; c.State != cluster.ReadyReady || c.Required != 1 || len(c.Confirmed) != 1 {
		t.Fatalf("R9 content state: %+v", c)
	}
	if a := d2.Archive; a == nil || a.State != cluster.ReadyReady || a.Required != 1 || a.Live == "" {
		t.Fatalf("archive state: %+v", a)
	}
	if res, err := n.DatasetWait(ctx, ScopeLocal, "photos", apiv0.DatasetWaitRequest{Target: v2.Root, Condition: apiv0.WaitArchive, Timeout: "5s"}); err != nil || !res.Satisfied || res.Live == "" {
		t.Fatalf("archive-ready wait: %+v %v", res, err)
	}

	// Exact old bytes by TID, label and time; the head by name.
	mustCat(t, n, "photos/a.txt", "version two")
	mustCat(t, n, "photos@"+v1.TID+"/a.txt", "version one")
	if _, err := n.DatasetLabel(ctx, ScopeLocal, "photos", apiv0.VersionLabelRequest{Version: v1.TID, Label: "release-1"}); err != nil {
		t.Fatal(err)
	}
	mustCat(t, n, "photos@release-1/a.txt", "version one")
	mustCat(t, n, "photos@"+v1.Time.Format(time.RFC3339Nano)+"/a.txt", "version one")
	if _, err := catString(t, n, "photos@2001-01-01/a.txt"); !isCode(err, apiv0.CodeNotFound) {
		t.Fatalf("a time before every version must not fall back: %v", err)
	}
	if _, err := n.DatasetLabel(ctx, ScopeLocal, "photos", apiv0.VersionLabelRequest{Version: v2.TID, Label: "release-1"}); !isCode(err, apiv0.CodeConflict) {
		t.Fatalf("duplicate label: %v", err)
	}

	// get -o of an old version writes its exact bytes.
	out := filepath.Join(t.TempDir(), "restore")
	var gr apiv0.GetResult
	if err := runOp(t, n, func() (*apiv0.Operation, error) {
		return n.Get(ctx, apiv0.GetRequest{Target: "photos@release-1", Output: out})
	}, &gr); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "a.txt")); string(b) != "version one" || gr.CID != v1.Root {
		t.Fatalf("restored %q from %s", b, gr.CID)
	}
	if _, err := os.Stat(filepath.Join(out, "b.txt")); !os.IsNotExist(err) {
		t.Fatal("restore of v1 contains v2's file")
	}

	// Gateway: /c/<ds>@<version>/<path> redirects to the immutable tree URL.
	gw := httptest.NewServer(gateway.New(n.Content))
	defer gw.Close()
	res, _ := gwGet(t, gw, "/c/photos@release-1/a.txt")
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/tree/"+v1.Root+"/a.txt" {
		t.Fatalf("gateway /c/: %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	diff, err := n.DatasetDiff(ctx, ScopeLocal, "photos", v1.TID, v2.TID)
	if err != nil || !slices.Equal(diff.Changed, []string{"a.txt"}) || !slices.Equal(diff.Added, []string{"b.txt"}) || len(diff.Removed) != 0 {
		t.Fatalf("diff = %+v, %v", diff, err)
	}

	// Rollback appends a new head and version; v1's identity is unchanged.
	rb, err := n.DatasetRollback(ctx, ScopeLocal, "photos", apiv0.RollbackRequest{Version: "release-1"})
	if err != nil {
		t.Fatal(err)
	}
	if rb.Head.Target != v1.Root || rb.Version == nil || rb.Version.RestoredFrom != v1.TID || rb.Version.TID == v1.TID || rb.Metadata.Signer == "" {
		t.Fatalf("rollback = %+v %+v", rb.Head, rb.Version)
	}
	mustCat(t, n, "photos/a.txt", "version one")
	vs := avail(t, n, "photos")
	if len(vs) != 3 || vs[v1.TID].Root != v1.Root || vs[v1.TID].Label != "release-1" || !vs[rb.Version.TID].Head {
		t.Fatalf("versions after rollback: %+v", vs)
	}

	// Restart: history, labels and availability persist (one commit per head + version).
	r := n.Repo
	n.Close()
	n2, err := NewLocal(r)
	if err != nil {
		t.Fatal(err)
	}
	defer n2.Close()
	vs = avail(t, n2, "photos")
	if len(vs) != 3 || vs[v2.TID].Availability != apiv0.AvailComplete || vs[v1.TID].Label != "release-1" {
		t.Fatalf("after restart: %+v", vs)
	}
	mustCat(t, n2, "photos@"+v2.TID+"/b.txt", "new file")
}

// Retention and honest availability (0047): a version outside the live set that never
// completed is skipped; prune reports free and shared bytes and releases only what no
// other dataset references; pruned and missing versions are never claimed retrievable;
// labeling missing content needs explicit reacquisition.
func TestArchiveRetentionAvailability(t *testing.T) {
	ctx := context.Background()
	n := newLocal(t)
	other, src := t.TempDir(), t.TempDir()
	// Another dataset references the shared file.
	if _, err := n.DatasetCreate(ctx, ScopeLocal, apiv0.DatasetCreateRequest{Name: "keep", Mirror: true}); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, other, map[string]string{"shared.txt": "shared bytes"})
	if res, err := add(t, n, apiv0.AddRequest{Path: other, Recursive: true}); err != nil {
		t.Fatal(err)
	} else if _, err := n.DatasetUpdate(ctx, ScopeLocal, "keep", apiv0.DatasetUpdateRequest{Target: res.CID}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.DatasetCreate(ctx, ScopeLocal, apiv0.DatasetCreateRequest{Name: "logs", History: &apiv0.HistoryPolicy{KeepLast: 1}}); err != nil {
		t.Fatal(err)
	}
	// A head whose content is not held here: observed, never complete.
	missing := "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"
	d0, err := n.DatasetUpdate(ctx, ScopeLocal, "logs", apiv0.DatasetUpdateRequest{Target: missing, Kind: "blob"})
	if err != nil {
		t.Fatal(err)
	}
	if d0.Version.Availability != apiv0.AvailObserved {
		t.Fatalf("never-held head: %+v", d0.Version)
	}
	if _, err := catString(t, n, "logs@"+d0.Version.TID); !isCode(err, apiv0.CodeUnavailable) {
		t.Fatalf("an observed version claimed retrievable: %v", err)
	}
	writeFiles(t, src, map[string]string{"log.txt": "first log", "shared.txt": "shared bytes"})
	v1 := importDir(t, n, "logs", src).Version
	writeFiles(t, src, map[string]string{"log.txt": "second log"})
	v2 := importDir(t, n, "logs", src).Version
	vs := avail(t, n, "logs")
	if a := vs[d0.Version.TID]; a.Availability != apiv0.AvailSkipped || a.Live {
		t.Fatalf("superseded never-completed version: %+v", a)
	}
	if a := vs[v1.TID]; a.Live || a.Availability != apiv0.AvailComplete {
		t.Fatalf("v1 outside keep_last 1 but still held: %+v", a)
	}

	rep, err := n.DatasetPrune(ctx, ScopeLocal, "logs", apiv0.PruneRequest{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Candidates) != 1 || rep.Candidates[0].TID != v1.TID || rep.FreeBytes < int64(len("first log")) || rep.SharedKept < int64(len("shared bytes")) {
		t.Fatalf("dry run: %+v", rep)
	}
	mustCat(t, n, "logs@"+v1.TID+"/log.txt", "first log") // a dry run changes nothing
	if rep, err = n.DatasetPrune(ctx, ScopeLocal, "logs", apiv0.PruneRequest{}); err != nil || !slices.Equal(rep.Pruned, []string{v1.TID}) {
		t.Fatalf("prune: %+v %v", rep, err)
	}
	if _, err := n.Store.GC(ctx, store.GCOptions{}); err != nil {
		t.Fatal(err)
	}
	vs = avail(t, n, "logs")
	if a := vs[v1.TID]; a.Availability != apiv0.AvailPruned {
		t.Fatalf("pruned version: %+v", a)
	}
	_, err = catString(t, n, "logs@"+v1.TID+"/log.txt")
	var ae *apiv0.Error
	if !errors.As(err, &ae) || ae.Code != apiv0.CodeUnavailable || ae.Details["availability"] != apiv0.AvailPruned {
		t.Fatalf("pruned version read: %v", err)
	}
	// A time selecting the pruned version reports it, never a neighbour.
	if _, err := catString(t, n, "logs@"+v1.Time.Format(time.RFC3339Nano)+"/log.txt"); !errors.As(err, &ae) || ae.Details["version"] != v1.TID {
		t.Fatalf("time selection of a pruned version: %v", err)
	}
	if _, err := n.DatasetRollback(ctx, ScopeLocal, "logs", apiv0.RollbackRequest{Version: v1.TID}); !isCode(err, apiv0.CodeUnavailable) {
		t.Fatalf("rollback to pruned content: %v", err)
	}
	// Shared references survive: the other dataset and the live version still read.
	mustCat(t, n, "keep/shared.txt", "shared bytes")
	mustCat(t, n, "logs@"+v2.TID+"/shared.txt", "shared bytes")

	// Labeling missing content needs explicit reacquisition and stays honest.
	if _, err := n.DatasetLabel(ctx, ScopeLocal, "logs", apiv0.VersionLabelRequest{Version: v1.TID, Label: "audit"}); !isCode(err, apiv0.CodeUnavailable) {
		t.Fatalf("label of pruned content without --reacquire: %v", err)
	}
	l, err := n.DatasetLabel(ctx, ScopeLocal, "logs", apiv0.VersionLabelRequest{Version: v1.TID, Label: "audit", Reacquire: true})
	if err != nil {
		t.Fatal(err)
	}
	if !l.Version.Live || l.Version.Availability == apiv0.AvailComplete || !strings.Contains(strings.Join(l.Version.Reasons, ","), "label") {
		t.Fatalf("reacquired version: %+v", l.Version)
	}
	if _, err := catString(t, n, "logs@audit/log.txt"); !isCode(err, apiv0.CodeUnavailable) {
		t.Fatalf("pending reacquisition claimed retrievable: %v", err)
	}
	// Re-adding the same content completes it again.
	writeFiles(t, src, map[string]string{"log.txt": "first log"})
	if _, err := add(t, n, apiv0.AddRequest{Path: src, Recursive: true}); err != nil {
		t.Fatal(err)
	}
	if a := avail(t, n, "logs")[v1.TID]; a.Availability != apiv0.AvailComplete {
		t.Fatalf("reacquired content: %+v", a)
	}
	mustCat(t, n, "logs@audit/log.txt", "first log")
}

// Budget pressure (C5, S7): an import that does not fit is refused with the shortfall;
// labeled and held versions stay pinned and readable, nothing is pruned to make room.
func TestArchiveBudgetPressure(t *testing.T) {
	ctx := context.Background()
	r, _, err := repo.Open(filepath.Join(t.TempDir(), "nu"))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"store.limit": "1M", "store.free_reserve": "0"} {
		if err := r.ConfigSet(k, v); err != nil {
			t.Fatal(err)
		}
	}
	n, err := NewLocal(r)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if _, err := n.DatasetCreate(ctx, ScopeLocal, apiv0.DatasetCreateRequest{Name: "photos", History: &apiv0.HistoryPolicy{KeepLast: 1}}); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	writeFiles(t, src, map[string]string{"a": strings.Repeat("a", 300<<10)})
	v1 := importDir(t, n, "photos", src).Version
	if _, err := n.DatasetLabel(ctx, ScopeLocal, "photos", apiv0.VersionLabelRequest{Version: v1.TID, Label: "safe"}); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, src, map[string]string{"a": strings.Repeat("b", 300<<10)})
	v2 := importDir(t, n, "photos", src).Version
	writeFiles(t, src, map[string]string{"a": strings.Repeat("c", 2<<20)})
	if _, err := add(t, n, apiv0.AddRequest{Path: src, Recursive: true}); !isCode(err, apiv0.CodeInsufficientStorage) {
		t.Fatalf("over-budget import: %v", err)
	}
	if _, err := n.Store.GC(ctx, store.GCOptions{}); err != nil {
		t.Fatal(err)
	}
	vs := avail(t, n, "photos")
	for _, tid := range []string{v1.TID, v2.TID} {
		if vs[tid].Availability != apiv0.AvailComplete {
			t.Fatalf("version %s under pressure: %+v", tid, vs[tid])
		}
	}
	mustCat(t, n, "photos@safe/a", strings.Repeat("a", 300<<10))
}
