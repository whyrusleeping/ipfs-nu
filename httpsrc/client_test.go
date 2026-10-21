package httpsrc_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whyrusleeping/nu/httpsrc"
)

func limits() httpsrc.Options {
	return httpsrc.Options{MaxBytes: 1024, Timeout: time.Second, IdleTimeout: time.Second}
}
func localConfig() httpsrc.Config {
	return httpsrc.Config{AllowHTTP: true, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
}
func discard(int64) (io.Writer, error) { return io.Discard, nil }

func TestSSRF(t *testing.T) {
	var dials atomic.Int32
	cfg := httpsrc.Config{DialContext: func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("unexpected connection")
	}}
	c := httpsrc.New(cfg)
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "0.0.0.0", "224.0.0.1", "100.64.0.1", "::1", "::", "fe80::1", "fc00::1", "ff02::1", "::ffff:8.8.8.8", "64:ff9b::a00:1", "64:ff9b::7f00:1", "64:ff9b:1::a00:1"} {
		_, err := c.Fetch(context.Background(), "https://"+net.JoinHostPort(ip, "443"), discard, limits())
		if !errors.Is(err, httpsrc.ErrPolicy) {
			t.Errorf("%s: %v", ip, err)
		}
	}
	_, err := c.Fetch(context.Background(), "http://public.test", discard, limits())
	if !errors.Is(err, httpsrc.ErrPolicy) {
		t.Fatal(err)
	}
	if dials.Load() != 0 {
		t.Fatal("policy allowed a connection")
	}
	// Local tests explicitly opt in to just the test server address.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()
	if _, err := httpsrc.New(localConfig()).Fetch(context.Background(), srv.URL, discard, limits()); err != nil {
		t.Fatal(err)
	}
}

func TestRebindingRedirectsAndAllowlist(t *testing.T) {
	for _, mode := range []string{"rebind", "unlisted", "private", "chain", "limit", "empty-list"} {
		t.Run(mode, func(t *testing.T) {
			var lookups, dials, requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := requests.Add(1)
				target := "http://allowed.test/next"
				switch mode {
				case "unlisted":
					target = "http://secret.test/"
				case "private":
					target = "http://127.0.0.1/"
				case "chain":
					if n == 2 {
						target = "http://secret.test/"
					}
				}
				http.Redirect(w, r, target, http.StatusTemporaryRedirect)
			}))
			defer srv.Close()
			cfg := httpsrc.Config{AllowHTTP: true, MaxRedirects: 2,
				LookupIP: func(ctx context.Context, host string) ([]netip.Addr, error) {
					n := lookups.Add(1)
					if host != "allowed.test" {
						t.Errorf("DNS leak to %s", host)
					}
					ip := "8.8.8.8"
					if mode == "rebind" && n > 1 {
						ip = "127.0.0.1"
					}
					return []netip.Addr{netip.MustParseAddr(ip)}, nil
				},
				// Test-only routing of the checked public literal into an in-process server.
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					dials.Add(1)
					if addr != "8.8.8.8:80" {
						t.Errorf("unchecked dial %s", addr)
					}
					return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(srv.URL, "http://"))
				},
			}
			opts := limits()
			opts.Hosts = []string{"allowed.test"}
			if mode == "private" {
				opts.Hosts = append(opts.Hosts, "127.0.0.1") // allowlisting never bypasses IP policy
			}
			if mode == "empty-list" {
				opts.Hosts = []string{}
			}
			_, err := httpsrc.New(cfg).Fetch(context.Background(), "http://allowed.test/", discard, opts)
			if !errors.Is(err, httpsrc.ErrPolicy) {
				t.Fatal(err)
			}
			want := int32(1)
			switch mode {
			case "chain":
				want = 2
			case "limit":
				want = 3
			case "empty-list":
				want = 0
			}
			if dials.Load() != want {
				t.Fatalf("connections: got %d want %d", dials.Load(), want)
			}
			if mode == "rebind" && lookups.Load() != 2 {
				t.Fatal("did not re-resolve at next connect")
			}
		})
	}
}

func TestResume(t *testing.T) {
	date := "Wed, 21 Oct 2015 07:28:00 GMT"
	old := "Wed, 21 Oct 2015 07:26:00 GMT"
	for _, tc := range []struct {
		name, etag, lm      string
		resume, ignore, bad bool
	}{
		{name: "strong", etag: `"v1"`, resume: true},
		{name: "weak", etag: `W/"v1"`},
		{name: "absent"},
		{name: "last-modified", lm: old, resume: true},
		{name: "recent-date", lm: date},
		{name: "weak-with-old-date", etag: `W/"v1"`, lm: old},
		{name: "ignored-range", etag: `"v1"`, resume: true, ignore: true},
		{name: "bad-range", etag: `"v1"`, resume: true, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Date", date)
				w.Header().Set("Content-Type", "application/octet-stream")
				if tc.etag != "" {
					w.Header().Set("ETag", tc.etag)
				}
				if tc.lm != "" {
					w.Header().Set("Last-Modified", tc.lm)
				}
				if requests.Add(1) == 1 {
					w.Header().Set("Content-Length", "6")
					_, _ = io.WriteString(w, "abc")
					return
				}
				if tc.resume {
					validator := tc.etag
					if validator == "" {
						validator = tc.lm
					}
					if r.Header.Get("Range") != "bytes=3-" || r.Header.Get("If-Range") != validator {
						t.Error("missing resume headers", r.Header)
					}
					if !tc.ignore {
						span := "bytes 3-5/6"
						if tc.bad {
							span = "bytes 2-4/6"
						}
						w.Header().Set("Content-Range", span)
						w.WriteHeader(http.StatusPartialContent)
						_, _ = io.WriteString(w, "def")
						return
					}
				} else if r.Header.Get("Range") != "" || r.Header.Get("If-Range") != "" {
					t.Error("unsafe resume", r.Header)
				}
				_, _ = io.WriteString(w, "abcdef")
			}))
			defer srv.Close()
			c := httpsrc.New(localConfig())
			var buf bytes.Buffer
			calls := 0
			sink := func(offset int64) (io.Writer, error) {
				calls++
				if offset == 0 {
					buf.Reset()
				} else if int64(buf.Len()) != offset {
					t.Fatal("wrong offset")
				}
				return &buf, nil
			}
			first, err := c.Fetch(context.Background(), srv.URL, sink, limits())
			if err == nil || first.Size != 3 || buf.String() != "abc" {
				t.Fatalf("first: %+v %q %v", first, buf.String(), err)
			}
			opts := limits()
			opts.Resume = &first
			result, err := c.Fetch(context.Background(), srv.URL, sink, opts)
			if tc.bad {
				if !errors.Is(err, httpsrc.ErrResponse) || calls != 1 {
					t.Fatal("invalid range reached sink", err)
				}
				return
			}
			if err != nil || result.Size != 6 || buf.String() != "abcdef" || result.FinalURL != srv.URL || result.ContentType != "application/octet-stream" {
				t.Fatalf("result: %+v %q %v", result, buf.String(), err)
			}
		})
	}
}

func TestBoundsAndDemotion(t *testing.T) {
	for _, mode := range []string{"declared-size", "streamed-size", "idle", "overall"} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "declared-size" {
					w.Header().Set("Content-Length", "20")
				}
				w.WriteHeader(http.StatusOK)
				if mode != "declared-size" {
					w.(http.Flusher).Flush()
				}
				if mode == "idle" || mode == "overall" {
					<-r.Context().Done()
					return
				}
				_, _ = io.WriteString(w, strings.Repeat("x", 20))
			}))
			defer srv.Close()
			opts := limits()
			opts.MaxBytes = 10
			if mode == "idle" {
				opts.IdleTimeout = 30 * time.Millisecond
			}
			if mode == "overall" {
				opts.Timeout = 30 * time.Millisecond
			}
			var buf bytes.Buffer
			_, err := httpsrc.New(localConfig()).Fetch(context.Background(), srv.URL, func(int64) (io.Writer, error) { return &buf, nil }, opts)
			if err == nil || buf.Len() > 10 {
				t.Fatalf("unbounded fetch: %d, %v", buf.Len(), err)
			}
			if strings.HasSuffix(mode, "size") && !errors.Is(err, httpsrc.ErrTooLarge) {
				t.Fatal(err)
			}
		})
	}
	c := httpsrc.New(httpsrc.Config{})
	c.ReportVerificationFailure("https://EXAMPLE.com/a")
	if !c.Demoted("https://example.com:443/b") || c.Demoted("https://example.com:444/a") || c.Demoted("https://other.test/a") {
		t.Fatal("demotion not scoped to origin")
	}
	for i := 0; i < 300; i++ {
		c.ReportVerificationFailure(fmt.Sprintf("https://host%d.test/", i))
	}
	if c.Demoted("https://example.com/") || !c.Demoted("https://host299.test/") {
		t.Fatal("scoreboard eviction failed")
	}
}
