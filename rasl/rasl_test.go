package rasl_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/core/cid"
	"github.com/whyrusleeping/nu/core/drisl"
	"github.com/whyrusleeping/nu/httpsrc"
	"github.com/whyrusleeping/nu/rasl"
	"github.com/whyrusleeping/nu/store"
)

// URL parsing follows the spec: CID authority, hint query values filtered to valid
// https hosts, path ignored; formatting round-trips; the legacy @dasl/rasl form parses.
func TestParse(t *testing.T) {
	c := cid.New(cid.Raw, cid.Sum(cid.SHA256, []byte("kitten")))
	u, err := rasl.Parse("rasl://" + c.String() + "/?hint=berjon.com&hint=Bsky.App.&hint=bad host&hint=-x.y&hint=10.0.0.1:8443&hint=[2001:db8::1]:443&hint=berjon.com&x=1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"berjon.com", "bsky.app", "10.0.0.1:8443", "[2001:db8::1]:443"}
	if u.CID != c || strings.Join(u.Hints, " ") != strings.Join(want, " ") {
		t.Fatalf("parsed %+v", u)
	}
	if s := u.String(); s != "rasl://"+c.String()+"/?hint=berjon.com&hint=bsky.app&hint=10.0.0.1%3A8443&hint=%5B2001%3Adb8%3A%3A1%5D%3A443" {
		t.Fatalf("formatted %s", s)
	}
	if back, err := rasl.Parse(u.String()); err != nil || back.String() != u.String() {
		t.Fatalf("round trip %+v %v", back, err)
	}
	for _, bad := range []string{"rasl://", "rasl://notacid/?hint=a", "https://x/", "rasl://" + c.String()[:10]} {
		if _, err := rasl.Parse(bad); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
	if u, err := rasl.Parse("rasl://" + c.String() + "/some/path"); err != nil || len(u.Hints) != 0 {
		t.Fatalf("path form: %+v %v", u, err)
	}
	if u, err := rasl.Parse("web+rasl://" + c.String() + ";berjon.com,bumblefudge.com/"); err != nil || strings.Join(u.Hints, ",") != "berjon.com,bumblefudge.com" || u.CID != c {
		t.Fatalf("legacy form: %+v %v", u, err)
	}
}

type raslServer struct {
	srv     *httptest.Server
	objects map[string][]byte
	hits    atomic.Int32
}

// serve answers /.well-known/rasl/<cid> like @dasl/rasl's handler; alter rewrites the
// response for failure modes.
func serve(t *testing.T, alter func(w http.ResponseWriter, r *http.Request, data []byte) bool) *raslServer {
	s := &raslServer{objects: map[string][]byte{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		c, ok := strings.CutPrefix(r.URL.Path, rasl.WellKnown)
		data, held := s.objects[c]
		if !ok || !held {
			http.NotFound(w, r)
			return
		}
		if alter != nil && alter(w, r, data) {
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *raslServer) hint() string { return strings.TrimPrefix(s.srv.URL, "http://") }

func (s *raslServer) add(c cid.CID, data []byte) { s.objects[c.String()] = data }

func testConfig(t *testing.T) (rasl.Config, *store.Store) {
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	client := httpsrc.New(httpsrc.Config{AllowHTTP: true, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	return rasl.Config{Client: client, Store: st, Scheme: "http", Dir: t.TempDir(), Timeout: 10 * time.Second, IdleTimeout: 5 * time.Second}, st
}

// FetchObject races the hints, verifies by CID before storing (SHA-256 raw and DRISL
// whole objects, BLAKE3 blobs of any size), follows a redirect to a plain file, demotes
// a host that serves wrong bytes and falls back to the next hint, and reports not-found
// only when every host says 404.
func TestFetchObject(t *testing.T) {
	cfg, st := testConfig(t)
	ctx := context.Background()
	small := []byte("hello RASL")
	rawSHA := cid.New(cid.Raw, cid.Sum(cid.SHA256, small))
	node, nodeData, _ := drisl.Sum(map[string]any{"src": rawSHA, "n": 1})
	big := bytes.Repeat([]byte("blake3 blob "), 100<<10) // 1.2 MiB, over the inline limit
	rawB3 := cid.New(cid.Raw, cid.Sum(cid.BLAKE3, big))

	good := serve(t, nil)
	good.add(rawSHA, small)
	good.add(node, nodeData)
	good.add(rawB3, big)
	empty := serve(t, nil)
	liar := serve(t, func(w http.ResponseWriter, r *http.Request, data []byte) bool {
		_, _ = w.Write(append([]byte("not "), data...))
		return true
	})
	liar.add(rawSHA, small)
	redirecting := serve(t, func(w http.ResponseWriter, r *http.Request, data []byte) bool {
		if strings.HasPrefix(r.URL.Path, rasl.WellKnown) {
			http.Redirect(w, r, "/kitten.jpg", http.StatusFound) // treated as 307
			return true
		}
		return false
	})
	redirecting.add(rawSHA, small)
	redirecting.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/kitten.jpg" {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(small)
			return
		}
		http.Redirect(w, r, "/kitten.jpg", http.StatusFound)
	})

	// Empty and good hosts raced: the good one serves all three objects.
	src := rasl.NewSource(cfg, []string{empty.hint(), good.hint()})
	for _, c := range []cid.CID{rawSHA, node, rawB3} {
		if err := src.FetchObject(ctx, c, 0); err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		if _, ok, err := st.Has(c); err != nil || !ok {
			t.Fatalf("%s not held after fetch: %v", c, err)
		}
	}
	if data, err := st.Get(node); err != nil || !bytes.Equal(data, nodeData) {
		t.Fatalf("node bytes %q %v", data, err)
	}
	if ents, _ := os.ReadDir(cfg.Dir); len(ents) != 0 {
		t.Fatalf("staging left behind: %v", ents)
	}

	// Only 404s: NotFound.
	var se *rasl.SourceError
	err := rasl.NewSource(cfg, []string{empty.hint()}).FetchObject(ctx, cid.New(cid.Raw, cid.Sum(cid.SHA256, []byte("missing"))), 0)
	if !errors.As(err, &se) || !se.NotFound() {
		t.Fatalf("missing everywhere: %v", err)
	}

	// Wrong bytes alone: mismatch, nothing stored, origin demoted.
	missing := cid.New(cid.Raw, cid.Sum(cid.SHA256, []byte("liar only")))
	liar.add(missing, []byte("liar only"))
	err = rasl.NewSource(cfg, []string{liar.hint()}).FetchObject(ctx, missing, 0)
	if !errors.Is(err, rasl.ErrMismatch) {
		t.Fatalf("liar: %v", err)
	}
	if _, ok, _ := st.Has(missing); ok {
		t.Fatal("mismatched bytes were stored")
	}
	if !cfg.Client.Demoted("http://" + liar.hint() + "/") {
		t.Fatal("liar not demoted")
	}
	// A demoted liar and a redirecting good host: the redirect target serves the bytes.
	other := cid.New(cid.Raw, cid.Sum(cid.SHA256, []byte("second")))
	liar.add(other, []byte("second"))
	redirecting.add(other, []byte("second"))
	redirecting.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/second.txt" {
			_, _ = w.Write([]byte("second"))
			return
		}
		http.Redirect(w, r, "/second.txt", http.StatusMovedPermanently)
	})
	before := liar.hits.Load()
	if err := rasl.NewSource(cfg, []string{liar.hint(), redirecting.hint()}).FetchObject(ctx, other, 6); err != nil {
		t.Fatalf("redirect: %v", err)
	}
	if liar.hits.Load() != before {
		t.Fatal("demoted host was asked although another hint served the object")
	}
	// A known size caps the download: a host sending more than the authenticated size fails.
	long := cid.New(cid.Raw, cid.Sum(cid.SHA256, []byte("tiny")))
	liar.add(long, []byte("tiny"))
	if err := rasl.NewSource(cfg, []string{liar.hint()}).FetchObject(ctx, long, 4); !errors.Is(err, httpsrc.ErrTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
}

// Hints to private, loopback or link-local addresses never reach DNS or a dial; a
// hostname that resolves there is refused too (rebinding).
func TestSSRF(t *testing.T) {
	var dials atomic.Int32
	client := httpsrc.New(httpsrc.Config{
		LookupIP: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
		},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("unexpected connection")
		}})
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := cid.New(cid.Raw, cid.Sum(cid.SHA256, []byte("x")))
	src := rasl.NewSource(rasl.Config{Client: client, Store: st, Timeout: time.Second, IdleTimeout: time.Second},
		[]string{"127.0.0.1:8171", "10.0.0.1", "169.254.169.254", "[::1]", "[fe80::1]:443", "rebinds.example"})
	err = src.FetchObject(context.Background(), c, 0)
	var se *rasl.SourceError
	if !errors.As(err, &se) || len(se.Attempts) != 6 {
		t.Fatalf("%v", err)
	}
	for _, a := range se.Attempts {
		if !errors.Is(a.Err, httpsrc.ErrPolicy) {
			t.Fatalf("%s: %v", a.Hint, a.Err)
		}
	}
	if dials.Load() != 0 {
		t.Fatal("a denied hint was dialed")
	}
}

// Opt-in live check: NU_LIVE_RASL=rasl://<cid>/?hint=<host> fetches over the real
// network and verifies by CID. (The spec's berjon.com example answered 404 when this
// test was written, so no default host is known to be live.)
func TestLive(t *testing.T) {
	raw := os.Getenv("NU_LIVE_RASL")
	if raw == "" {
		t.Skip("set NU_LIVE_RASL=rasl://<cid>/?hint=<host> to run")
	}
	u, err := rasl.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := rasl.NewSource(rasl.Config{Client: httpsrc.New(httpsrc.Config{}), Store: st, Timeout: time.Minute, IdleTimeout: 20 * time.Second}, u.Hints)
	if err := src.FetchObject(context.Background(), u.CID, 0); err != nil {
		t.Fatal(err)
	}
	f, err := st.OpenBlob(u.CID.Multihash())
	if err == nil {
		defer f.Close()
		n, _ := io.Copy(io.Discard, io.NewSectionReader(f, 0, int64(f.Size)))
		t.Logf("fetched %s: %d bytes", u.CID, n)
	}
}
