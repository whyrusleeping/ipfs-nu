package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
)

// objectContent adds RASL objects to the fake: the fake's files by CID.
type objectContent struct{ *fakeContent }

func (o objectContent) OpenObject(ctx context.Context, c string) (File, error) {
	for _, ff := range o.files {
		if ff.cid == c && ff.kind != KindSymlink {
			return readSeekCloser{bytes.NewReader(ff.data)}, nil
		}
	}
	return nil, apiv0.Errorf(apiv0.CodeNotFound, "%s is not held", c)
}

// /.well-known/rasl/<cid> serves a held object as an octet-stream download with Range
// and immutable caching, ignores the query string, has no CORS header on loopback
// (N29), answers 404 for an unheld CID without fetching, 400 for a non-CID, and refuses
// anything below the prefix.
func TestRASLRoute(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(New(objectContent{f}))
	defer srv.Close()
	notes := f.files["notes.txt"].cid
	res, body := get(t, srv, "", "/.well-known/rasl/"+notes+"?download=1", map[string]string{"Origin": "https://evil.example"})
	if res.StatusCode != 200 || body != "0123456789" || res.Header.Get("Content-Type") != "application/octet-stream" ||
		res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Cache-Control") != immutableCache ||
		res.Header.Get("ETag") != `"`+notes+`"` || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") ||
		res.Header.Get("Access-Control-Allow-Origin") != "" || res.Header.Get("Set-Cookie") != "" || res.Header.Get("Vary") != "" {
		t.Fatalf("rasl: %d %s %q", res.StatusCode, res.Header, body)
	}
	res, body = get(t, srv, "", "/.well-known/rasl/"+notes, map[string]string{"Range": "bytes=2-4"})
	if res.StatusCode != 206 || body != "234" {
		t.Fatalf("range: %d %q", res.StatusCode, body)
	}
	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/.well-known/rasl/"+notes, nil)
	if res, err := srv.Client().Do(req); err != nil || res.StatusCode != 200 || res.Header.Get("Content-Length") != "10" {
		t.Fatalf("head: %v %v", res, err)
	}
	// An HTML file is still an octet-stream download here, whatever its type.
	if res, _ := get(t, srv, "", "/.well-known/rasl/"+f.files["index.html"].cid, nil); res.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("html via rasl: %s", res.Header)
	}
	if res, _ := get(t, srv, "", "/.well-known/rasl/"+fakeCID(77), nil); res.StatusCode != 404 {
		t.Fatalf("unheld: %d", res.StatusCode)
	}
	if res, _ := get(t, srv, "", "/.well-known/rasl/notacid", nil); res.StatusCode != 400 {
		t.Fatalf("non-cid: %d", res.StatusCode)
	}
	for _, p := range []string{"/.well-known/rasl/", "/.well-known/rasl/" + notes + "/x", "/.well-known/"} {
		if res, _ := get(t, srv, "", p, nil); res.StatusCode != 404 {
			t.Fatalf("%s: %d", p, res.StatusCode)
		}
	}
	// Without object support the route says so instead of guessing.
	plain := httptest.NewServer(New(f))
	defer plain.Close()
	if res, _ := get(t, plain, "", "/.well-known/rasl/"+notes, nil); res.StatusCode != 501 {
		t.Fatalf("no object content: %d", res.StatusCode)
	}
}
