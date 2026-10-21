package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/daemon"
)

// A producer on "another machine" pushes through the producer listener with a push
// token (uploads, --wait); the listener refuses missing, foreign-scoped and revoked
// tokens and unlisted Host names, and serves nothing but the push routes. The operator
// removes the item and reads the push log on the local API.
func TestProducerPushCLI(t *testing.T) {
	old := node.HeadInterval
	node.HeadInterval = 50 * time.Millisecond
	t.Cleanup(func() { node.HeadInterval = old })
	r := loopbackRepo(t, t.TempDir())
	for k, v := range map[string]string{"api.producer.trusted_network": "true", "api.producer.hosts": `["127.0.0.1"]`} {
		if err := r.ConfigSet(k, v); err != nil {
			t.Fatal(err)
		}
	}
	d, err := daemon.Start(context.Background(), daemon.Options{Repo: r, APIListen: "127.0.0.1:0", GatewayListen: "127.0.0.1:0",
		ProducerListen: "127.0.0.1:0", Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	exec := func(args ...string) (int, []byte, string) {
		var out, errb bytes.Buffer
		code := run(append([]string{"--nu-path", r.Root(), "--json"}, args...), &out, &errb)
		return code, out.Bytes(), errb.String()
	}
	nu := func(args ...string) []byte {
		t.Helper()
		code, out, errs := exec(args...)
		if code != apiv0.ExitOK {
			t.Fatalf("%v: exit %d: %s", args, code, errs)
		}
		return out
	}
	nu("cluster", "init", "lab")
	nu("cluster", "dataset", "create", "s2", "--collection")
	nu("cluster", "dataset", "create", "other", "--collection")
	var tok struct {
		Token string `json:"token"`
	}
	json.Unmarshal(nu("api", "token", "create", "pilot", "--collection", "s2"), &tok)
	var otherTok struct {
		Token string `json:"token"`
	}
	json.Unmarshal(nu("api", "token", "create", "elsewhere", "--collection", "other"), &otherTok)

	dir := t.TempDir()
	scene := filepath.Join(dir, "scene")
	os.MkdirAll(filepath.Join(scene, "R10m"), 0o755)
	os.WriteFile(filepath.Join(scene, "R10m", "B02.jp2"), []byte("band 2"), 0o644)
	os.WriteFile(filepath.Join(scene, "visual.tif"), []byte("pixels"), 0o644)
	meta := filepath.Join(dir, "meta.json")
	os.WriteFile(meta, []byte(`{"stac": {"id": "S2B_1", "properties": {"eo:cloud_cover": 12.5}}}`), 0o644)

	prod := "http://" + d.ProducerAddr()
	put := []string{"cluster", "collection", "put", "s2", "2026/10/21/S2B_1", "--meta", meta, scene, "--upload", "--wait"}
	if code, _, _ := exec(append([]string{"--api", prod}, put...)...); code != apiv0.ExitForbidden {
		t.Fatalf("push without a token: exit %d", code)
	}
	if code, _, _ := exec(append([]string{"--api", prod, "--token", otherTok.Token}, put...)...); code != apiv0.ExitForbidden {
		t.Fatalf("push with another collection's token: exit %d", code)
	}
	var p apiv0.CollectionPush
	json.Unmarshal(nu(append([]string{"--api", prod, "--token", tok.Token}, put...)...), &p)
	if p.State != apiv0.PushWritten || p.Head == "" || p.Entry == "" {
		t.Fatalf("push %+v", p)
	}
	var again apiv0.CollectionPush
	json.Unmarshal(nu(append([]string{"--api", prod, "--token", tok.Token}, put...)...), &again)
	if !again.Duplicate || again.ID != p.ID {
		t.Fatalf("resend %+v, want push %s again", again, p.ID)
	}
	var ls apiv0.Listing
	json.Unmarshal(nu("ls", p.Entry), &ls)
	if len(ls.Entries) != 2 || ls.Entries[0].Name != "R10m" || ls.Entries[1].Name != "visual.tif" {
		t.Fatalf("item directory %+v", ls)
	}

	for _, c := range []struct {
		host, path string
		want       int
	}{
		{"evil.example", "/cluster/collections/s2/pushes", http.StatusForbidden},
		{"127.0.0.1", "/status", http.StatusNotFound},
		{"127.0.0.1", "/cluster/collections/s2/pushes", http.StatusOK},
	} {
		req, _ := http.NewRequest("GET", prod+apiv0.Prefix+c.path, nil)
		req.Host = c.host
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Errorf("GET %s with Host %s: %d, want %d", c.path, c.host, res.StatusCode, c.want)
		}
	}

	var rm apiv0.CollectionPush
	json.Unmarshal(nu("cluster", "collection", "rm", "s2", "2026/10/21/S2B_1", "--wait"), &rm)
	if rm.State != apiv0.PushRemoved || rm.Head == "" {
		t.Fatalf("rm %+v", rm)
	}
	var log apiv0.Page[apiv0.CollectionPush]
	json.Unmarshal(nu("cluster", "collection", "pushes", "s2"), &log)
	if len(log.Items) != 1 || log.Items[0].State != apiv0.PushRemoved {
		t.Fatalf("push log %+v", log)
	}
	nu("api", "token", "rm", "pilot")
	if code, _, _ := exec(append([]string{"--api", prod, "--token", tok.Token}, put...)...); code != apiv0.ExitForbidden {
		t.Fatalf("push with a revoked token: exit %d", code)
	}
}
