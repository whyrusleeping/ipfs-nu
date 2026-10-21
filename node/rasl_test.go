package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/rasl"
)

// raslSeed serves a holder's held objects at /.well-known/rasl/<cid> the way the gateway
// does (octet-stream, no fetch of unheld CIDs).
func raslSeed(t *testing.T, holder *Local) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := strings.CutPrefix(r.URL.Path, rasl.WellKnown)
		if !ok {
			http.NotFound(w, r)
			return
		}
		root, err := cid.Parse(c)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		f, err := holder.content.Object(root)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, f)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// `nu get rasl://<cid>/?hint=…` on a node without networking fetches a whole tree
// (header, directory nodes, file nodes, blobs) from the hinted hosts alone, surviving a
// dead hint and ignoring the hint that is not an https host; cat works on the same
// target; a CID no hint has is not_found.
func TestGetRASL(t *testing.T) {
	holder, getter := newLocal(t), newLocal(t)
	ctx := context.Background()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	big := bytes.Repeat([]byte("big blob data "), 20<<10) // 280 KiB: a real blob file
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "big.bin"), big, 0o644)
	res, err := add(t, holder, apiv0.AddRequest{Path: dir, Recursive: true, Pin: true})
	if err != nil {
		t.Fatal(err)
	}
	seed := raslSeed(t, holder)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	IngestHTTP = func(c *httpsrc.Config) {
		c.AllowHTTP, c.AllowedIPs = true, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	}
	RASLHook = func(c *rasl.Config) { c.Scheme = "http" }
	t.Cleanup(func() { IngestHTTP, RASLHook = nil, nil })
	hint := func(srv *httptest.Server) string { return strings.TrimPrefix(srv.URL, "http://") }

	target := "rasl://" + res.CID + "/?hint=" + hint(dead) + "&hint=not%20a%20host&hint=" + hint(seed)
	out := t.TempDir()
	o, err := getter.Get(ctx, apiv0.GetRequest{Target: target, Dir: out})
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
	// Everything is held now, so share works and cat reads the file through the same link.
	if sl, err := getter.Share(ctx, apiv0.ShareRequest{Target: res.CID}); err != nil || sl.Link == "" {
		t.Fatalf("share: %+v %v", sl, err)
	}
	rc, err := getter.Cat(ctx, "rasl://"+res.CID+"/?hint="+hint(seed))
	if err == nil {
		rc.Close()
	}
	if !isCode(err, apiv0.CodeInvalidArgument) { // the root is a directory; the path is ignored by RASL
		t.Fatalf("cat of a directory: %v", err)
	}
	// A CID nobody serves.
	missing := "rasl://" + strings.Replace(res.CID, res.CID[len(res.CID)-4:], "aaaa", 1) + "/?hint="
	if _, err := getter.Ls(ctx, missing+hint(seed)); !isCode(err, apiv0.CodeNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// Hints are checked by the SSRF policy: a private address is refused without a dial.
	IngestHTTP = nil
	_, err = getter.Ls(ctx, missing+"10.0.0.1")
	var ae *apiv0.Error
	if !errors.As(err, &ae) || ae.Code != apiv0.CodeUnavailable || !strings.Contains(fmt.Sprint(ae.Details["cause"]), "denied") {
		t.Fatalf("private hint: %v", err)
	}
}
