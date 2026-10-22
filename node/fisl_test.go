package node

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/fisl"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/rasl"
)

// fislIndexer answers every valid CID with one record pointing at seed's RASL URL for
// it (what an ATFISL-style index of a RASL server looks like), in jsonl.
func fislIndexer(t *testing.T, seed *httptest.Server, hits *atomic.Int32) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		rest, ok := strings.CutPrefix(r.URL.Path, fisl.PathPrefix)
		c, format, _ := strings.Cut(rest, "/")
		if !ok || format != "jsonl" {
			http.NotFound(w, r)
			return
		}
		if _, err := cid.Parse(c); err != nil {
			http.Error(w, "bad cid", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/jsonl")
		rec, _ := json.Marshal(map[string]any{"publisher": "did:web:seed.example", "addrs": []map[string]string{{"url": seed.URL + rasl.WellKnown + c}}})
		w.Write(append(rec, '\n'))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// With `fisl.indexers` configured, `nu get <cid>` on a node without networking finds
// every object of a tree through the indexer and fetches it from the located URLs; a
// CID the indexer has no location for is not_found; a private cluster member never
// asks the indexer.
func TestGetFISL(t *testing.T) {
	holder, getter := newLocal(t), newLocal(t)
	ctx := context.Background()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	big := bytes.Repeat([]byte("big blob data "), 20<<10)
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "big.bin"), big, 0o644)
	res, err := add(t, holder, apiv0.AddRequest{Path: dir, Recursive: true, Pin: true})
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	seed := raslSeed(t, holder)
	ix := fislIndexer(t, seed, &hits)
	IngestHTTP = func(c *httpsrc.Config) {
		c.AllowHTTP, c.AllowedIPs = true, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	}
	FISLHook = func(c *fisl.Config) { c.AllowHTTP = true }
	t.Cleanup(func() { IngestHTTP, FISLHook = nil, nil })
	if err := getter.Repo.ConfigSet("fisl.indexers", `["not a url", "`+ix.URL+`/"]`); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	o, err := getter.Get(ctx, apiv0.GetRequest{Target: res.CID, Dir: out})
	if err != nil {
		t.Fatal(err)
	}
	final, err := getter.WaitOperation(ctx, o.ID, 0)
	if err != nil || final.State != apiv0.StateSucceeded {
		t.Fatalf("get: %v %+v", err, final)
	}
	var gr apiv0.GetResult
	json.Unmarshal(final.Result, &gr)
	if !gr.Fetched || gr.Files != 2 || gr.Bytes != int64(5+len(big)) {
		t.Fatalf("result %+v", gr)
	}
	if b, _ := os.ReadFile(filepath.Join(gr.Output, "sub", "big.bin")); !bytes.Equal(b, big) {
		t.Fatalf("extracted big.bin: %d bytes", len(b))
	}
	if hits.Load() == 0 {
		t.Fatal("the indexer was never asked")
	}
	// A CID the seed does not hold: the indexer claims a location, the seed says 404.
	missing := strings.Replace(res.CID, res.CID[len(res.CID)-4:], "aaaa", 1)
	if _, err := getter.Ls(ctx, missing); !isCode(err, apiv0.CodeNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// A private cluster member does not query indexers: nothing can serve the CID.
	if _, err := getter.ClusterInit(ctx, apiv0.ClusterInitRequest{Name: "lab", Mode: "private"}); err != nil {
		t.Fatal(err)
	}
	before := hits.Load()
	if _, err := getter.Ls(ctx, missing); err == nil || isCode(err, apiv0.CodeNotFound) {
		t.Fatalf("private member: %v", err)
	}
	if hits.Load() != before {
		t.Fatal("a private cluster member asked the indexer")
	}
}
