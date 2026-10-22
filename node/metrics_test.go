package node

import (
	"strings"
	"testing"
	"time"
)

func TestMetricsExposition(t *testing.T) {
	var m Metrics
	m.Counter("nu_x_total", "X.", Labels("b", "2", "a", `q"\`), 3)
	m.Counter("nu_x_total", "X.", Labels("a", "1"), 4)
	m.Gauge("nu_g", "G.", "", 0.5)
	h := NewHistogram([]float64{0.1, 1})
	h.Observe(50 * time.Millisecond)
	h.Observe(2 * time.Second)
	m.Histogram("nu_h_seconds", "H.", Labels("k", "v"), h.Snapshot())
	var b strings.Builder
	if _, err := m.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	want := "# HELP nu_x_total X.\n# TYPE nu_x_total counter\n" +
		"nu_x_total{a=\"q\\\"\\\\\",b=\"2\"} 3\nnu_x_total{a=\"1\"} 4\n" +
		"# HELP nu_g G.\n# TYPE nu_g gauge\nnu_g 0.5\n" +
		"# HELP nu_h_seconds H.\n# TYPE nu_h_seconds histogram\n" +
		"nu_h_seconds_bucket{k=\"v\",le=\"0.1\"} 1\nnu_h_seconds_bucket{k=\"v\",le=\"1\"} 1\nnu_h_seconds_bucket{k=\"v\",le=\"+Inf\"} 2\n" +
		"nu_h_seconds_sum{k=\"v\"} 2.05\nnu_h_seconds_count{k=\"v\"} 2\n"
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}
