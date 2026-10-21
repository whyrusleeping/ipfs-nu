package gateway

import (
	"bytes"
	"context"
	"encoding/base32"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
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
		},
		dirs: map[string]bool{"": true, "sub": true},
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

func (f *fakeContent) List(ctx context.Context, info *Info) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		prefix := ""
		if info.Name != "" {
			prefix = info.Name + "/"
		}
		for p, ff := range f.files {
			if rest, ok := strings.CutPrefix(p, prefix); ok && !strings.Contains(rest, "/") {
				if !yield(Entry{Name: rest, Kind: ff.kind, CID: ff.cid, Size: int64(len(ff.data))}, nil) {
					return
				}
			}
		}
		for d := range f.dirs {
			if rest, ok := strings.CutPrefix(d, prefix); ok && d != "" && !strings.Contains(rest, "/") {
				if !yield(Entry{Name: rest, Kind: KindDir, CID: fakeCID(9)}, nil) {
					return
				}
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
