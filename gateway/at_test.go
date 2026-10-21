package gateway

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
)

// fakeAt is a held snapshot whose record and names carry HTML and script.
type fakeAt struct{ fakeContent }

const (
	fakeDID = "did:plc:aaaaaaaaaaaaaaaaaaaaaaaa"
	fakeRev = "3lw5relfkkt2h"
)

func (f *fakeAt) AtSnapshot(ctx context.Context, did, rev string) (*AtSnapshot, error) {
	if did != fakeDID || (rev != "" && rev != fakeRev) {
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "no snapshot of %s", did)
	}
	return &AtSnapshot{DID: did, Rev: fakeRev, Commit: fakeCID(9)}, nil
}

func (f *fakeAt) AtCollections(ctx context.Context, s *AtSnapshot) ([]AtCollection, error) {
	return []AtCollection{{Name: "app.bsky.feed.post", Count: 1}, {Name: "<script>alert(1)</script>", Count: 1}}, nil
}

func (f *fakeAt) AtRecords(ctx context.Context, s *AtSnapshot, collection string) ([]AtRecord, error) {
	return []AtRecord{{RKey: "3abc", CID: fakeCID(8), Size: 60}, {RKey: "<img src=x onerror=alert(1)>", CID: fakeCID(7), Size: 1}}, nil
}

func (f *fakeAt) AtRecordJSON(ctx context.Context, s *AtSnapshot, collection, rkey string) ([]byte, string, error) {
	if rkey != "3abc" {
		return nil, "", apiv0.Errorf(apiv0.CodeNotFound, "no record %s", rkey)
	}
	return []byte(`{"$type":"app.bsky.feed.post","text":"<script>alert(1)</script><img src=x onerror=alert(2)>"}`), fakeCID(8), nil
}

// N30 (G5): records render as inert JSON with the listing CSP, never as HTML; listings
// escape hostile names and link only syntactically valid collections and record keys;
// /at/ has no site origin; nothing is served for an unheld DID.
func TestAtRoutesInert(t *testing.T) {
	f := &fakeAt{*newFake()}
	srv := httptest.NewServer(New(f))
	defer srv.Close()

	res, body := get(t, srv, "", "/at/"+fakeDID+"/app.bsky.feed.post/3abc", nil)
	ct := res.Header.Get("Content-Type")
	if res.StatusCode != 200 || !strings.HasPrefix(ct, "application/json") || res.Header.Get("Content-Security-Policy") != listingCSP ||
		res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Content-Disposition") != "" ||
		!strings.Contains(body, `"text":"<script>`) || res.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("record: %d %v %s", res.StatusCode, res.Header, body)
	}
	// Fixed-rev view is immutable; same bytes.
	res, _ = get(t, srv, "", "/at/"+fakeDID+"@"+fakeRev+"/app.bsky.feed.post/3abc", nil)
	if res.StatusCode != 200 || res.Header.Get("Cache-Control") != immutableCache {
		t.Fatalf("record at rev: %d %v", res.StatusCode, res.Header)
	}
	// Listings: HTML escaped, hostile names unlinked, CSP present.
	res, body = get(t, srv, "", "/at/"+fakeDID+"/", nil)
	if res.StatusCode != 200 || strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;") ||
		!strings.Contains(body, `href="app.bsky.feed.post/"`) || res.Header.Get("Content-Security-Policy") != listingCSP {
		t.Fatalf("collections: %d %s", res.StatusCode, body)
	}
	res, body = get(t, srv, "", "/at/"+fakeDID+"/app.bsky.feed.post/", nil)
	if res.StatusCode != 200 || strings.Contains(body, "<img") || !strings.Contains(body, `href="3abc"`) || strings.Contains(body, `href="%3Cimg`) {
		t.Fatalf("records: %d %s", res.StatusCode, body)
	}
	if res, _ := get(t, srv, "", "/at/"+fakeDID, nil); res.StatusCode != 301 || res.Header.Get("Location") != "/at/"+fakeDID+"/" {
		t.Fatalf("listing without slash: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	// Bad inputs and unheld snapshots.
	for p, want := range map[string]int{
		"/at/notadid/":                                  400,
		"/at/" + fakeDID + "@bad/":                      400,
		"/at/" + fakeDID + "/not_an_nsid/":              400,
		"/at/" + fakeDID + "/app.bsky.feed.post/3abc/x": 404,
		"/at/" + fakeDID + "//app.bsky.feed.post/":      400,
		"/at/did:plc:bbbbbbbbbbbbbbbbbbbbbbbb/":         404,
		"/at/" + fakeDID + "@2222222222222/":            404,
	} {
		if res, body := get(t, srv, "", p, nil); res.StatusCode != want || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") {
			t.Errorf("%s: %d %s", p, res.StatusCode, body)
		}
	}
	// No site origin for /at/: on <cid>.localhost the path is a tree path.
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	if res, _ := get(t, srv, f.root+".localhost:"+port, "/at/"+fakeDID+"/", nil); res.StatusCode != 404 {
		t.Fatalf("/at/ on a site origin: %d", res.StatusCode)
	}
	// A Content without snapshots answers not_implemented, never fetches.
	plain := httptest.NewServer(New(newFake()))
	defer plain.Close()
	if res, _ := get(t, plain, "", "/at/"+fakeDID+"/", nil); res.StatusCode != 501 {
		t.Fatalf("no AtContent: %d", res.StatusCode)
	}
}
