package gateway

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The public listener (0043; operations.md P1–P6, N28/N29): Host must be the domain or
// <cid>.<domain>; only public roots are served and a held private root reads as unheld;
// site URLs are https://<cid>.<domain>/; RASL adds CORS here and only here; /at/ and
// /api/ do not exist; the request limiter answers 503 beyond its bounds.
func TestPublicListener(t *testing.T) {
	f := newFake()
	notes, index := f.files["notes.txt"].cid, f.files["index.html"].cid
	public := map[string]bool{f.root: true, notes: true, index: true}
	lookups := 0
	h := NewPublic(namedContent{objectContent{f}, f.root, &lookups}, PublicOptions{Domain: "seed.example", Serves: func(c string) bool { return public[c] }})
	srv := httptest.NewTLSServer(h)
	defer srv.Close()
	srv.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	site := f.root + ".seed.example"

	// Host rules (P5): the domain, with a trailing dot or :443, and <cid>.<domain>; nothing else.
	for _, host := range []string{"seed.example", "SEED.example.", "seed.example:443"} {
		if res, body := get(t, srv, host, "/tree/"+f.root+"/", nil); res.StatusCode != 200 || !strings.Contains(body, "https://"+site+"/") {
			t.Fatalf("host %q: %d %q", host, res.StatusCode, body)
		}
	}
	for _, host := range []string{"127.0.0.1", "localhost", "evil.example", "seed.example:8443", f.root + ".evil.example",
		"x." + f.root + ".seed.example", f.root + ".localhost", "evil.seed.example"} {
		if res, _ := get(t, srv, host, "/tree/"+f.root+"/", nil); res.StatusCode != 403 {
			t.Fatalf("host %q: %d", host, res.StatusCode)
		}
	}

	// P2: a held root that is not public is indistinguishable from an unheld one, on every route.
	photo := f.files["sub/photo.png"].cid // held (a blob root in the fake), not public
	for _, p := range []string{"/tree/" + photo + "/", "/blob/" + photo, "/.well-known/rasl/" + photo} {
		res, body := get(t, srv, "seed.example", p, nil)
		unheld, ubody := get(t, srv, "seed.example", strings.Replace(p, photo, fakeCID(77), 1), nil)
		if res.StatusCode != 404 || unheld.StatusCode != 404 || strings.Replace(body, photo, fakeCID(77), 1) != ubody {
			t.Fatalf("private root on %s: %d %q vs unheld %d %q", p, res.StatusCode, body, unheld.StatusCode, ubody)
		}
	}
	if res, _ := get(t, srv, photo+".seed.example", "/", nil); res.StatusCode != 404 {
		t.Fatalf("private site origin: %d", res.StatusCode)
	}

	// Public root: bare origin lists, active content is a download with an https site Link, Range works.
	res, _ := get(t, srv, "seed.example", "/tree/"+f.root+"/index.html", nil)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/octet-stream" ||
		!strings.Contains(res.Header.Get("Link"), "<https://"+site+"/index.html>") || res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("active on public bare origin: %d %s", res.StatusCode, res.Header)
	}
	if res, body := get(t, srv, "seed.example", "/tree/"+f.root+"/notes.txt", map[string]string{"Range": "bytes=2-4"}); res.StatusCode != 206 || body != "234" ||
		res.Header.Get("Cross-Origin-Resource-Policy") != "" { // public content may be embedded anywhere
		t.Fatalf("range: %d %q", res.StatusCode, body)
	}
	// Site origin renders index.html; no CORS, no cookies; directory redirect stays on the origin.
	if res, body := get(t, srv, site, "/", nil); res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(body, "<h1>hi</h1>") || res.Header.Get("Access-Control-Allow-Origin") != "" || res.Header.Get("Set-Cookie") != "" {
		t.Fatalf("site: %d %s %q", res.StatusCode, res.Header, body)
	}
	if res, _ := get(t, srv, site, "/sub", nil); res.StatusCode != 301 || res.Header.Get("Location") != "/sub/" {
		t.Fatalf("site redirect: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	// N29 (public half): RASL for a public object has CORS; the header appears on no other route.
	res, body := get(t, srv, "seed.example", "/.well-known/rasl/"+notes, map[string]string{"Origin": "https://verifier.example"})
	if res.StatusCode != 200 || body != "0123456789" || res.Header.Get("Access-Control-Allow-Origin") != "*" ||
		res.Header.Get("Content-Type") != "application/octet-stream" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("public rasl: %d %s", res.StatusCode, res.Header)
	}
	if res, _ := get(t, srv, "seed.example", "/.well-known/rasl/"+photo, nil); res.StatusCode != 404 || res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("rasl private: %d %s", res.StatusCode, res.Header)
	}
	// P1/N28: no API; P4: no /at/, and (N44) no /c/ or /nu/: they would reveal dataset
	// roots (X-Nu-Root) and resolve names live for anonymous clients.
	for _, p := range []string{"/api/v0/status", "/api/", "/at/did:plc:abc/", "/at/did:plc:abc", "/c/lab", "/c/lab/x", "/nu/alice.example/site", "/nu/alice.example/site/x"} {
		if res, _ := get(t, srv, "seed.example", p, nil); res.StatusCode != 404 || res.Header.Get("X-Nu-Root") != "" {
			t.Fatalf("%s on public listener: %d %s", p, res.StatusCode, res.Header)
		}
	}
	if lookups != 0 {
		t.Fatalf("public listener resolved %d datasets/names", lookups)
	}
	for _, m := range []string{http.MethodPost, http.MethodOptions, http.MethodDelete} {
		req, _ := http.NewRequest(m, srv.URL+"/tree/"+f.root+"/", nil)
		req.Host = "seed.example"
		req.Header.Set("Origin", "https://evil.example")
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 400 || res.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("%s: %d %s", m, res.StatusCode, res.Header)
		}
	}
	// The root page says what this is and is not cached.
	if res, body := get(t, srv, "seed.example", "/", nil); res.StatusCode != 200 || !strings.Contains(body, "seed.example") || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("root: %d %s", res.StatusCode, res.Header)
	}
}

// Limit: beyond the per-address (or total) bound a request is 503 with Retry-After and
// never reaches the handler; once in-flight requests finish, requests are served again.
func TestPublicLimit(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(Limit(slow, 2, 1))
	defer srv.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if res, err := http.Get(srv.URL + "/"); err == nil {
			res.Body.Close()
		}
	}()
	<-started
	// Same client IP: the per-address bound of 1 is full.
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 503 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("over per-ip bound: %d %s", res.StatusCode, res.Header)
	}
	close(release)
	<-done
	res, err = http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("after release: %d", res.StatusCode)
	}
}

// ParseDomain accepts public DNS names with an optional port and refuses what P5 rules
// out (localhost tricks, IP literals, single labels).
func TestParseDomain(t *testing.T) {
	for in, want := range map[string]string{"Seed.Example": "seed.example", "seed.example:443": "seed.example", "seed.example:8443": "seed.example:8443"} {
		if got, err := ParseDomain(in); err != nil || got != want {
			t.Fatalf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "localhost", "cid.localhost", "127.0.0.1", "[::1]:443", "seed", "https://seed.example", "seed.example/x", "seed.example:0", ".seed.example"} {
		if got, err := ParseDomain(in); err == nil {
			t.Fatalf("%q accepted as %q", in, got)
		}
	}
}

// namedContent adds dataset and name lookups (always resolving to root) to the fake.
type namedContent struct {
	objectContent
	root    string
	lookups *int
}

func (n namedContent) ResolveDataset(ctx context.Context, name, version string) (string, error) {
	*n.lookups++
	return n.root, nil
}

func (n namedContent) ResolveName(ctx context.Context, authority, name string, held bool) (string, string, error) {
	*n.lookups++
	return n.root, "pds", nil
}

// Public listener bounds (0077 item 6, N46): a Range of more than MaxRangeParts parts is
// answered whole; listings are paged (MaxListingRows, ?from=) with a per-page ETag that
// revalidates to 304 (G9); a client that stops reading is cut off after StallTimeout.
func TestPublicBounds(t *testing.T) {
	f := newFake()
	h := NewPublic(objectContent{f}, PublicOptions{Domain: "seed.example", Serves: func(c string) bool { return c == f.root }})
	srv := httptest.NewServer(Limit(h, 0, 0))
	defer srv.Close()
	notes := "/tree/" + f.root + "/notes.txt"
	if res, _ := get(t, srv, "seed.example", notes, map[string]string{"Range": "bytes=0-0,2-2"}); res.StatusCode != 206 {
		t.Fatalf("two ranges: %d", res.StatusCode)
	}
	many := "bytes=0-0" + strings.Repeat(",1-1", MaxRangeParts)
	if res, body := get(t, srv, "seed.example", notes, map[string]string{"Range": many}); res.StatusCode != 200 || body != "0123456789" {
		t.Fatalf("%d ranges: %d %q", MaxRangeParts+1, res.StatusCode, body)
	}

	old := MaxListingRows
	MaxListingRows = 2
	t.Cleanup(func() { MaxListingRows = old })
	dir := "/tree/" + f.root + "/"
	res, body := get(t, srv, "seed.example", dir, nil)
	// Root entries in name order: "bad\x00name", "evil.example", "index.html", "link", ...
	if res.StatusCode != 200 || !strings.Contains(body, "evil.example") || strings.Contains(body, ">index.html<") ||
		!strings.Contains(body, `href="?from=index.html"`) || res.Header.Get("ETag") != `"`+fakeCID(9)+`"` {
		t.Fatalf("first page: %d %s\n%s", res.StatusCode, res.Header, body)
	}
	res2, body := get(t, srv, "seed.example", dir+"?from=index.html", nil)
	if res2.StatusCode != 200 || !strings.Contains(body, ">index.html<") || !strings.Contains(body, ">link<") || strings.Contains(body, "evil.example") ||
		res2.Header.Get("ETag") == "" || res2.Header.Get("ETag") == res.Header.Get("ETag") {
		t.Fatalf("second page: %d %s\n%s", res2.StatusCode, res2.Header, body)
	}
	if res, _ := get(t, srv, "seed.example", dir, map[string]string{"If-None-Match": res.Header.Get("ETag")}); res.StatusCode != 304 {
		t.Fatalf("listing If-None-Match: %d", res.StatusCode)
	}

	oldStall := StallTimeout
	StallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { StallTimeout = oldStall })
	werr := make(chan error, 1)
	big := httptest.NewServer(Limit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 32<<10)
		for {
			if _, err := w.Write(chunk); err != nil {
				werr <- err
				return
			}
		}
	}), 0, 0))
	defer big.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(big.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n") // and never read
	select {
	case <-werr:
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled reader held the response past StallTimeout")
	}
}
