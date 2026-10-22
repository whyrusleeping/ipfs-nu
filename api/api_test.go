package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
	"github.com/whyrusleeping/nu/node/repo"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	r, _, err := repo.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n, err := node.NewLocal(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	srv := httptest.NewServer(New(n, nil))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path string, hdr map[string]string) (*http.Response, *apiv0.Envelope) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var env apiv0.Envelope
	_ = json.NewDecoder(res.Body).Decode(&env)
	return res, &env
}

// A page in a browser (even one served by the loopback gateway) must not be able to
// operate the daemon, and a public name rebound to 127.0.0.1 must not reach it either.
func TestLoopbackProtections(t *testing.T) {
	srv := newTestServer(t)
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")

	cases := []struct {
		name string
		hdr  map[string]string
		code apiv0.Code
	}{
		{"foreign origin on POST", map[string]string{"Origin": "http://127.0.0.1:8171"}, apiv0.CodeForbidden},
		{"cross-site fetch on POST", map[string]string{"Sec-Fetch-Site": "cross-site"}, apiv0.CodeForbidden},
		{"same-site (gateway) fetch on POST", map[string]string{"Sec-Fetch-Site": "same-site"}, apiv0.CodeForbidden},
		{"non-loopback host", map[string]string{"Host": "nu.example.com:" + port}, apiv0.CodeForbidden},
		{"loopback host, wrong port", map[string]string{"Host": "127.0.0.1:1"}, apiv0.CodeForbidden},
		// The control: a plain loopback POST reaches the handler (the op does not exist).
		{"same-origin POST", map[string]string{"Origin": "http://127.0.0.1:" + port, "Sec-Fetch-Site": "same-origin"}, apiv0.CodeNotFound},
	}
	for _, tc := range cases {
		res, env := do(t, srv, http.MethodPost, apiv0.Prefix+"/operations/nope/cancel", tc.hdr)
		if env.Error == nil || env.Error.Code != tc.code {
			t.Errorf("%s: got status %d envelope %+v, want code %s", tc.name, res.StatusCode, env.Error, tc.code)
		}
		if res.StatusCode != apiv0.HTTPStatus(tc.code) {
			t.Errorf("%s: status %d, want %d", tc.name, res.StatusCode, apiv0.HTTPStatus(tc.code))
		}
		for h := range res.Header {
			if strings.HasPrefix(h, "Access-Control-") {
				t.Errorf("%s: CORS header %s must never be sent", tc.name, h)
			}
		}
		if env.Error != nil && env.Error.Action == "" {
			t.Errorf("%s: error without action", tc.name)
		}
	}

	// N8 (0077): GET/HEAD are refused cross-site too (/cat, /ls and /resolve fetch and
	// store); a same-origin or user-initiated (none) GET and a CLI GET reach the handler.
	for _, hdr := range []map[string]string{{"Origin": "http://evil.example"}, {"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"}, {"Origin": "http://127.0.0.1:8171"}, {"Origin": "null"}} {
		for _, m := range []string{http.MethodGet, http.MethodHead} {
			if res, _ := do(t, srv, m, apiv0.Prefix+"/status", hdr); res.StatusCode != http.StatusForbidden {
				t.Errorf("%s with %v: status %d, want 403", m, hdr, res.StatusCode)
			}
		}
	}
	for _, hdr := range []map[string]string{nil, {"Sec-Fetch-Site": "none"}, {"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:" + port}} {
		if res, env := do(t, srv, http.MethodGet, apiv0.Prefix+"/status", hdr); res.StatusCode != http.StatusOK || env.Error != nil {
			t.Errorf("GET with %v: status %d env %+v", hdr, res.StatusCode, env.Error)
		}
	}
	res, _ := do(t, srv, http.MethodGet, apiv0.Prefix+"/status", nil)
	if h := res.Header; h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" || h.Get("Set-Cookie") != "" {
		t.Errorf("response headers: %s", h)
	}
	// Preflights are refused rather than answered.
	res, _ = do(t, srv, http.MethodOptions, apiv0.Prefix+"/status", nil)
	if res.StatusCode != http.StatusForbidden || res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("OPTIONS: status %d, CORS %q", res.StatusCode, res.Header.Get("Access-Control-Allow-Origin"))
	}
	// A form submission (a browser that sends neither Origin nor Sec-Fetch-Site) cannot
	// deliver a body: only application/json is read, which forms cannot send.
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", ""} {
		for _, p := range []string{"/pins", "/dag"} {
			_, env := do(t, srv, http.MethodPost, apiv0.Prefix+p, map[string]string{"Content-Type": ct})
			if env.Error == nil || env.Error.Code != apiv0.CodeInvalidArgument {
				t.Errorf("POST %s with Content-Type %q: %+v, want invalid_argument", p, ct, env.Error)
			}
		}
	}
}

// The embedded OpenAPI document describes exactly the routes the server registers.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(OpenAPI(), &doc); err != nil {
		t.Fatal(err)
	}
	documented := map[Route]bool{}
	for p, methods := range doc.Paths {
		for m := range methods {
			documented[Route{strings.ToUpper(m), p}] = true
		}
	}
	for _, rt := range Routes {
		if !documented[rt] {
			t.Errorf("route %s %s missing from openapi.json", rt.Method, rt.Pattern)
		}
		delete(documented, rt)
	}
	for rt := range documented {
		t.Errorf("openapi.json documents %s %s, which is not served", rt.Method, rt.Pattern)
	}
}
