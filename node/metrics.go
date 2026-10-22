package node

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics builds a Prometheus text-format page (text/plain; version=0.0.4). Names
// carry the nu_ prefix and base units (bytes, seconds). Families are emitted in
// registration order, each once, so collectors must agree on a family's type.
type Metrics struct {
	order    []string
	families map[string]*family
}
type family struct {
	kind, help string
	samples    []string
}

// MetricsContentType is the exposition content type served at /metrics.
const MetricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// MetricsNode is implemented by nodes (and, through Local.Net, network stacks) that
// contribute families to the /metrics page.
type MetricsNode interface {
	Metrics(ctx context.Context, m *Metrics)
}

func (m *Metrics) family(name, kind, help string) *family {
	if m.families == nil {
		m.families = map[string]*family{}
	}
	f := m.families[name]
	if f == nil {
		f = &family{kind: kind, help: help}
		m.families[name] = f
		m.order = append(m.order, name)
	}
	return f
}

// Labels renders a label set deterministically (sorted by key, values escaped).
func Labels(kv ...string) string {
	if len(kv) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(kv[i+1])
		pairs = append(pairs, kv[i]+`="`+v+`"`)
	}
	sort.Strings(pairs)
	return "{" + strings.Join(pairs, ",") + "}"
}

// Counter adds a counter sample; name should end in _total.
func (m *Metrics) Counter(name, help, labels string, v float64) {
	m.family(name, "counter", help).samples = append(m.family(name, "counter", help).samples, name+labels+" "+format(v))
}

// Gauge adds a gauge sample.
func (m *Metrics) Gauge(name, help, labels string, v float64) {
	m.family(name, "gauge", help).samples = append(m.family(name, "gauge", help).samples, name+labels+" "+format(v))
}

// Histogram adds one histogram's cumulative buckets, sum and count.
func (m *Metrics) Histogram(name, help, labels string, h HistogramSnapshot) {
	f := m.family(name, "histogram", help)
	inner := strings.TrimSuffix(strings.TrimPrefix(labels, "{"), "}")
	bucket := func(le string, n uint64) {
		l := `le="` + le + `"`
		if inner != "" {
			l = inner + "," + l
		}
		f.samples = append(f.samples, name+"_bucket{"+l+"} "+format(float64(n)))
	}
	var cumulative uint64
	for i, b := range h.Bounds {
		cumulative += h.Counts[i]
		bucket(format(b), cumulative)
	}
	bucket("+Inf", h.Count)
	f.samples = append(f.samples, name+"_sum"+labels+" "+format(h.Sum), name+"_count"+labels+" "+format(float64(h.Count)))
}

func format(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return fmt.Sprintf("%g", v)
}

// WriteTo renders the page.
func (m *Metrics) WriteTo(w io.Writer) (int64, error) {
	var b strings.Builder
	for _, name := range m.order {
		f := m.families[name]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, f.help, name, f.kind)
		for _, s := range f.samples {
			b.WriteString(s)
			b.WriteByte('\n')
		}
	}
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

// Histogram is a concurrency-safe fixed-bucket histogram of seconds.
type Histogram struct {
	bounds []float64
	counts []atomic.Uint64
	count  atomic.Uint64
	sum    atomic.Uint64 // float64 bits updated under mu
	mu     sync.Mutex
}

// HistogramSnapshot is a Histogram's exported state for Metrics.Histogram.
type HistogramSnapshot struct {
	Bounds []float64
	Counts []uint64 // per bucket, not cumulative
	Count  uint64
	Sum    float64
}

// DurationBuckets suit network timings: 10 ms to 60 s.
var DurationBuckets = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// NewHistogram returns a histogram with the given upper bounds (ascending seconds).
func NewHistogram(bounds []float64) *Histogram {
	return &Histogram{bounds: bounds, counts: make([]atomic.Uint64, len(bounds))}
}

// Observe records one duration.
func (h *Histogram) Observe(d time.Duration) {
	v := d.Seconds()
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i].Add(1)
			break
		}
	}
	h.count.Add(1)
	h.mu.Lock()
	h.sum.Store(math.Float64bits(math.Float64frombits(h.sum.Load()) + v))
	h.mu.Unlock()
}

// Snapshot exports the current state.
func (h *Histogram) Snapshot() HistogramSnapshot {
	s := HistogramSnapshot{Bounds: h.bounds, Counts: make([]uint64, len(h.bounds)), Count: h.count.Load(), Sum: math.Float64frombits(h.sum.Load())}
	for i := range h.counts {
		s.Counts[i] = h.counts[i].Load()
	}
	return s
}

// Metrics implements MetricsNode: store basics plus whatever the network stack adds.
func (n *Local) Metrics(ctx context.Context, m *Metrics) {
	if st, err := n.Store.Stats(); err == nil {
		ss := storeStatus(st)
		m.Gauge("nu_store_objects", "Objects in the local store.", "", float64(ss.Objects))
		m.Gauge("nu_store_bytes", "Bytes held by the local store.", "", float64(ss.Bytes))
		m.Gauge("nu_store_pins", "Pins in the local store.", "", float64(ss.Pins))
		if ss.Limit > 0 {
			m.Gauge("nu_store_limit_bytes", "Configured store.limit.", "", float64(ss.Limit))
		}
		if ss.Free >= 0 {
			m.Gauge("nu_store_free_bytes", "Free space on the store's filesystem.", "", float64(ss.Free))
		}
		m.Gauge("nu_store_pressure", "1 when new content is refused for lack of space.", "", b2f(ss.Pressure))
	}
	if n.Net != nil {
		if mn, ok := n.Net.(MetricsNode); ok {
			mn.Metrics(ctx, m)
		}
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
