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
	"testing/fstest"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/node/daemon"
)

// The collection CLI over the API: create a collection with facets, dry-run and apply
// rules, list items by facet, locate an item, and scrape the metrics.
func TestCollectionCLI(t *testing.T) {
	r := loopbackRepo(t, t.TempDir())
	d, err := daemon.Start(context.Background(), daemon.Options{Repo: r, APIListen: "127.0.0.1:0", GatewayListen: "127.0.0.1:0", Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	nu := func(args ...string) []byte {
		t.Helper()
		var out, errb bytes.Buffer
		if code := run(append([]string{"--nu-path", r.Root(), "--json"}, args...), &out, &errb); code != apiv0.ExitOK {
			t.Fatalf("%v: exit %d: %s", args, code, errb.String())
		}
		return out.Bytes()
	}
	dir := t.TempDir()
	facets, rules := filepath.Join(dir, "facets.toml"), filepath.Join(dir, "rules.toml")
	os.WriteFile(facets, []byte("cloud = { ptr = \"/stac/properties/eo:cloud_cover\", type = \"number\" }\nsensed = { ptr = \"/stac/properties/datetime\", type = \"time\" }\n"), 0o644)
	os.WriteFile(rules, []byte("[[rule]]\ncloud = { max = 30 }\n"), 0o644)
	nu("cluster", "init", "lab")
	var ds apiv0.Dataset
	json.Unmarshal(nu("cluster", "dataset", "create", "s2", "--collection", "--facets", facets, "--expire", "sensed=400d"), &ds)
	if ds.Collection == nil || len(ds.Collection.Facets) != 2 || ds.Collection.Expire != "sensed=400d" || ds.Replicas != 0 {
		t.Fatalf("dataset %+v", ds)
	}

	st := d.Node.Store
	for i, cloud := range []float64{10, 90} {
		res, err := tree.BuildFS(fstest.MapFS{"visual.tif": {Data: []byte{byte(i)}}}, ".", tree.BuildOptions{Blobs: st, Nodes: st})
		if err != nil {
			t.Fatal(err)
		}
		mc, mb, _ := drisl.Sum(map[string]any{"stac": map[string]any{"properties": map[string]any{"datetime": "2026-10-20T10:00:00Z", "eo:cloud_cover": cloud}}})
		st.Put(mc, mb)
		if err := d.Node.Collections().Put("s2", []string{"a/clear", "a/cloudy"}[i], &tree.Entry{Kind: tree.KindDir, CID: res.Root, Size: res.Bytes, Meta: mc}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Node.Collections().Flush(context.Background(), "s2"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "items", func() bool {
		var s apiv0.CollectionStatus
		json.Unmarshal(nu("cluster", "collection", "status", "s2"), &s)
		return s.Items == 2
	})
	var dry apiv0.CollectionRules
	json.Unmarshal(nu("cluster", "collection", "rules", "s2", "--set", rules, "--dry-run"), &dry)
	if dry.Applied || len(dry.DryRun) != 1 || dry.DryRun[0].Items != 1 {
		t.Fatalf("dry run %+v", dry)
	}
	nu("cluster", "collection", "rules", "s2", "--set", rules)
	var page apiv0.Page[apiv0.CollectionItem]
	json.Unmarshal(nu("cluster", "collection", "items", "s2", "--facet", "cloud.max=30"), &page)
	if len(page.Items) != 1 || page.Items[0].Path != "a/clear" {
		t.Fatalf("items %+v", page)
	}
	waitFor(t, "held", func() bool {
		var it apiv0.CollectionItem
		json.Unmarshal(nu("cluster", "where", "s2/a/clear"), &it)
		return it.Local == "complete"
	})
	res, err := http.Get("http://" + d.APIAddr() + apiv0.Prefix + "/cluster/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), `nu_collection_items{collection="s2"} 2`) {
		t.Fatalf("metrics:\n%s", body)
	}
}
