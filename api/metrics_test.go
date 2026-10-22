package api

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// /metrics is Prometheus text on the loopback API listener: store basics without a
// network stack, and the same Host rules as the JSON API.
func TestMetricsEndpoint(t *testing.T) {
	srv := newTestServer(t)
	res, err := http.Get(srv.URL + MetricsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != node.MetricsContentType {
		t.Fatalf("%d %s: %s", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	for _, want := range []string{"# TYPE nu_store_objects gauge\nnu_store_objects 0\n", "nu_store_pressure 0\n"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	if res, env := do(t, srv, http.MethodGet, MetricsPath, map[string]string{"Host": "nu.example.com:" + port}); res.StatusCode != http.StatusForbidden || env.Error == nil || env.Error.Code != apiv0.CodeForbidden {
		t.Fatalf("non-loopback host reached /metrics: %d %+v", res.StatusCode, env.Error)
	}
}
