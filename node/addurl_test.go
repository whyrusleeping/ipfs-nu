package node

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/tree"
	"github.com/whyrusleeping/nu/httpsrc"
)

// `nu add https://…`: the bytes become a BLAKE3 file node named after the URL, the
// result carries the provenance, --meta records it under "source"; --hash sha256 keeps
// the 8 MiB limit.
func TestAddURL(t *testing.T) {
	data := bytes.Repeat([]byte("cog "), 3<<20) // 12 MiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"e1"`)
		w.Header().Set("Content-Type", "image/tiff")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	IngestHTTP = func(c *httpsrc.Config) {
		c.AllowHTTP, c.AllowedIPs = true, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	}
	t.Cleanup(func() { IngestHTTP = nil })
	n := newLocal(t)
	res, err := add(t, n, apiv0.AddRequest{URL: srv.URL + "/scenes/visual.tif", Pin: true, Meta: []byte(`{"stac": {"id": "x"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	blob := cid.New(cid.Raw, cid.Sum(cid.BLAKE3, data))
	if res.Name != "visual.tif" || res.Size != int64(len(data)) || res.Source == nil || res.Source.ETag != `"e1"` ||
		res.Source.ContentType != "image/tiff" || res.Entry == nil || res.Entry.CID != blob.String() {
		t.Fatalf("add result %+v (source %+v, entry %+v)", res, res.Source, res.Entry)
	}
	fn, err := n.Store.Get(cid.MustParse(res.CID))
	if err != nil {
		t.Fatal(err)
	}
	if node, err := tree.DecodeFileNode(fn); err != nil || node.Src != blob {
		t.Fatalf("file node %+v %v", node, err)
	}
	meta, err := n.DagGet(context.Background(), res.Meta)
	if err != nil || !strings.Contains(string(meta), `"etag":"\"e1\""`) || !strings.Contains(string(meta), `"url":"`+srv.URL) {
		t.Fatalf("meta %s %v", meta, err)
	}
	if _, err := add(t, n, apiv0.AddRequest{URL: srv.URL + "/big", Hash: "sha256"}); err == nil || !strings.Contains(err.Error(), "8 MiB") {
		t.Fatalf("sha256 import over 8 MiB: %v", err)
	}
}
