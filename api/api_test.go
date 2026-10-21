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
	srv := httptest.NewServer(New(node.NewLocal(r), nil))
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

	// A GET with a foreign Origin is harmless (no CORS headers, so the page cannot read it)
	// and must reach the handler.
	res, env := do(t, srv, http.MethodGet, apiv0.Prefix+"/status", map[string]string{"Origin": "http://evil.example"})
	if res.StatusCode != http.StatusOK || env.Error != nil {
		t.Errorf("GET with foreign origin: status %d env %+v", res.StatusCode, env.Error)
	}
	// Preflights are refused rather than answered.
	res, _ = do(t, srv, http.MethodOptions, apiv0.Prefix+"/status", nil)
	if res.StatusCode != http.StatusForbidden || res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("OPTIONS: status %d, CORS %q", res.StatusCode, res.Header.Get("Access-Control-Allow-Origin"))
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
