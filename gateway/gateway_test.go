package gateway

import (
	"bytes"
	"context"
	"encoding/base32"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
)

// fakeContent is an in-memory tree: one root directory with files and a subdirectory.
type fakeContent struct {
	root  string
	files map[string]fakeFile // path → file
	dirs  map[string]bool     // path → exists ("" is the root)
}

type fakeFile struct {
	cid, mime string
	data      []byte
	kind      Kind
}

var cidEnc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func fakeCID(seed byte) string {
	b := append([]byte{0x01, 0x55, 0x12, 0x20}, bytes.Repeat([]byte{seed}, 32)...)
	return "b" + cidEnc.EncodeToString(b)
}

func newFake() *fakeContent {
	return &fakeContent{
		root: fakeCID(1),
		files: map[string]fakeFile{
			"index.html":    {cid: fakeCID(2), mime: "text/html", data: []byte("<h1>hi</h1><script>1</script>"), kind: KindFile},
			"notes.txt":     {cid: fakeCID(3), mime: "text/plain; charset=utf-8", data: []byte("0123456789"), kind: KindFile},
			"sub/photo.png": {cid: fakeCID(4), mime: "image/png", data: []byte("PNG"), kind: KindFile},
			"link":          {cid: fakeCID(5), kind: KindSymlink},
			"bad\x00name":   {cid: fakeCID(6), mime: "text/plain", data: []byte("x"), kind: KindFile},
		},
		dirs: map[string]bool{"": true, "sub": true, "evil.example": true},
	}
}

func (f *fakeContent) Resolve(ctx context.Context, root, p string) (*Info, error) {
	if root != f.root {
		// A bare blob root.
		for _, ff := range f.files {
			if ff.cid == root && p == "" {
				return &Info{Kind: KindBlob, CID: ff.cid, Size: int64(len(ff.data))}, nil
			}
		}
		return nil, apiv0.Errorf(apiv0.CodeNotFound, "%s is not held", root)
	}
	if f.dirs[p] {
		return &Info{Kind: KindDir, CID: fakeCID(9), Name: p, Size: 42}, nil
	}
	if ff, ok := f.files[p]; ok {
		return &Info{Kind: ff.kind, CID: ff.cid, Name: p[strings.LastIndex(p, "/")+1:], Size: int64(len(ff.data)), Mime: ff.mime}, nil
	}
	return nil, apiv0.Errorf(apiv0.CodeNotFound, "no %q in %s", p, root)
}

type readSeekCloser struct{ *bytes.Reader }

func (readSeekCloser) Close() error { return nil }

func (f *fakeContent) Open(ctx context.Context, info *Info) (File, error) {
	for _, ff := range f.files {
		if ff.cid == info.CID {
			return readSeekCloser{bytes.NewReader(ff.data)}, nil
		}
	}
	return nil, apiv0.Errorf(apiv0.CodeNotFound, "no bytes for %s", info.CID)
}

// List yields in name order, as Content requires.
func (f *fakeContent) List(ctx context.Context, info *Info) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		prefix := ""
		if info.Name != "" {
			prefix = info.Name + "/"
		}
		var out []Entry
		for p, ff := range f.files {
			if rest, ok := strings.CutPrefix(p, prefix); ok && !strings.Contains(rest, "/") {
				out = append(out, Entry{Name: rest, Kind: ff.kind, CID: ff.cid, Size: int64(len(ff.data))})
			}
		}
		for d := range f.dirs {
			if rest, ok := strings.CutPrefix(d, prefix); ok && d != "" && !strings.Contains(rest, "/") {
				out = append(out, Entry{Name: rest, Kind: KindDir, CID: fakeCID(9)})
			}
		}
		slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
		for _, e := range out {
			if !yield(e, nil) {
				return
			}
		}
	}
}

func get(t *testing.T, srv *httptest.Server, host, path string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if host != "" {
		req.Host = host
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

// The bare origin lists and downloads; the <cid>.localhost origin renders a site; hosts
// that are neither are refused; files support Range.
func TestGatewayOrigins(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(New(f))
	defer srv.Close()
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	site := f.root + ".localhost:" + port

	// Bare origin: listing with a link to the site origin and the root metadata.
	res, body := get(t, srv, "", "/tree/"+f.root+"/", nil)
	if res.StatusCode != 200 || !strings.Contains(body, "notes.txt") || !strings.Contains(body, "http://"+site+"/") ||
		!strings.Contains(body, "42 bytes") || !strings.Contains(body, "symlink") || res.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("listing: %d %s\n%s", res.StatusCode, res.Header, body)
	}
	// N48: loopback responses (content and errors) cannot be embedded cross-site.
	for _, p := range []string{"/tree/" + f.root + "/", "/tree/" + f.root + "/notes.txt", "/tree/" + fakeCID(7) + "/"} {
		if res, _ := get(t, srv, "", p, nil); res.Header.Get("Cross-Origin-Resource-Policy") != "same-site" {
			t.Fatalf("%s: no CORP: %s", p, res.Header)
		}
	}
	// Missing trailing slash on a directory redirects.
	if res, _ := get(t, srv, "", "/tree/"+f.root, nil); res.StatusCode != 301 {
		t.Fatalf("dir without slash: %d", res.StatusCode)
	}
	// Active content on the bare origin is a download, with a Link to the site origin.
	res, _ = get(t, srv, "", "/tree/"+f.root+"/index.html", nil)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/octet-stream" ||
		!strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") ||
		!strings.Contains(res.Header.Get("Link"), "http://"+site+"/index.html") {
		t.Fatalf("active on bare origin: %s", res.Header)
	}
	// Inline-safe content is inline, with Range.
	res, body = get(t, srv, "", "/tree/"+f.root+"/notes.txt", map[string]string{"Range": "bytes=2-4"})
	if res.StatusCode != 206 || body != "234" || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("range: %d %q %s", res.StatusCode, body, res.Header)
	}
	// Blobs are downloads.
	res, _ = get(t, srv, "", "/blob/"+fakeCID(3), nil)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("blob: %d %s", res.StatusCode, res.Header)
	}
	// Symlinks are not served.
	if res, _ := get(t, srv, "", "/tree/"+f.root+"/link", nil); res.StatusCode != 422 {
		t.Fatalf("symlink: %d", res.StatusCode)
	}

	// Site origin: index.html renders as HTML; subdirectories list; traversal is refused.
	res, body = get(t, srv, site, "/", nil)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") || !strings.Contains(body, "<h1>hi</h1>") {
		t.Fatalf("site index: %d %s %q", res.StatusCode, res.Header, body)
	}
	if res, body := get(t, srv, site, "/sub/", nil); res.StatusCode != 200 || !strings.Contains(body, "photo.png") {
		t.Fatalf("site listing: %d %q", res.StatusCode, body)
	}
	if res, _ := get(t, srv, site, "/sub/../index.html", nil); res.StatusCode != 400 {
		t.Fatalf("traversal: %d", res.StatusCode)
	}
	// N15 on the bare origin too (0077): 400, never a cleaning redirect.
	for _, p := range []string{"/tree/" + f.root + "/sub/../index.html", "/tree/" + f.root + "/./notes.txt", "/tree/" + f.root + "/..",
		"/blob/../tree/" + f.root + "/", "/tree/" + f.root + "/%2e%2e/x"} {
		if res, _ := get(t, srv, "", p, nil); res.StatusCode != 400 || res.Header.Get("Location") != "" {
			t.Fatalf("bare-origin %s: %d %q", p, res.StatusCode, res.Header.Get("Location"))
		}
	}
	// A directory named like a host must not turn "//evil.example" into a redirect off
	// this origin; empty segments are refused outright.
	if res, _ := get(t, srv, site, "//evil.example", nil); res.StatusCode != 400 || res.Header.Get("Location") != "" {
		t.Fatalf("scheme-relative path: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res, _ := get(t, srv, site, "/sub", nil); res.StatusCode != 301 || res.Header.Get("Location") != "/sub/" {
		t.Fatalf("dir redirect: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	// Unsafe names are listed but not linked; the site origin sends no CSP for content.
	if res, body := get(t, srv, site, "/", map[string]string{"Accept": "text/html"}); res.Header.Get("Content-Security-Policy") != "" ||
		strings.Contains(body, "Content-Security-Policy") {
		t.Fatalf("site content must not carry the listing CSP: %s", res.Header)
	}
	if _, body := get(t, srv, "", "/tree/"+f.root+"/", nil); !strings.Contains(body, "bad") || strings.Contains(body, `href="bad`) {
		t.Fatalf("unsafe name must be shown unlinked:\n%s", body)
	}
	// Only GET and HEAD; no preflight, no CORS, no cookies.
	for _, m := range []string{http.MethodPost, http.MethodOptions, http.MethodPut} {
		req, _ := http.NewRequest(m, srv.URL+"/tree/"+f.root+"/", nil)
		req.Header.Set("Origin", "http://evil.example")
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 400 || res.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: %d %s", m, res.StatusCode, res.Header)
		}
	}
	if res, _ := get(t, srv, "", "/tree/"+f.root+"/notes.txt", map[string]string{"If-None-Match": `"` + fakeCID(3) + `"`}); res.StatusCode != 304 {
		t.Errorf("If-None-Match: %d", res.StatusCode)
	}

	// Host validation.
	for _, host := range []string{"nu.example.com:" + port, f.root + ".localhost:1", "evil.localhost:" + port, "127.0.0.1:1"} {
		if res, _ := get(t, srv, host, "/", nil); res.StatusCode != 403 {
			t.Errorf("host %q: %d, want 403", host, res.StatusCode)
		}
	}
	for _, host := range []string{"localhost:" + port, "127.0.0.1:" + port} {
		res, _ := get(t, srv, host, "/", nil)
		if res.StatusCode != 200 || res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Set-Cookie") != "" {
			t.Errorf("host %q: %d %s", host, res.StatusCode, res.Header)
		}
	}
	if res, _ := get(t, srv, "", "/tree/"+fakeCID(7)+"/", nil); res.StatusCode != 404 {
		t.Fatalf("unheld: %d", res.StatusCode)
	}
}

// Hostile or sloppy content types are sanitized, and only inert types render inline on
// the bare origin (operations.md G4, G7).
func TestMimeRules(t *testing.T) {
	for _, tc := range []struct{ mime, name, want string }{
		{"text/html; charset=x-evil", "a", "text/html"},
		{"text/plain; charset=UTF-8", "a", "text/plain; charset=utf-8"},
		{"foo", "a.png", "image/png"},
		{"", "a.unknownext", "application/octet-stream"},
		{"a/b/c", "a", "application/octet-stream"},
		{"text/html\r\nX-Injected: 1", "a", "application/octet-stream"},
	} {
		if got := sanitizeMime(tc.mime, tc.name); got != tc.want {
			t.Errorf("sanitizeMime(%q, %q) = %q, want %q", tc.mime, tc.name, got, tc.want)
		}
	}
	for mt, inline := range map[string]bool{
		"text/html": false, "image/svg+xml": false, "application/xml": false, "text/xml": false,
		"application/javascript": false, "application/pdf": false, "text/javascript": false,
		"text/plain; charset=utf-8": true, "image/png": true, "video/mp4": true, "application/json": true,
	} {
		if inlineSafe(mt) != inline {
			t.Errorf("inlineSafe(%q) = %v", mt, !inline)
		}
	}
}
