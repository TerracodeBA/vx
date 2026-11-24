// Package metrics is a small Prometheus registry: counters, gauges and
// histograms with labels, written in the Prometheus text format (version
// 0.0.4). It has no dependencies, so every package may record into it.
//
// Labels never carry repository, organization or person names: only
// bounded values like an RPC procedure, a route pattern, a status or a
// result. The metrics vx records live in vx.go.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds metrics and writes them out.
type Registry struct {
	mu      sync.Mutex
	metrics []metric
	names   map[string]bool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{names: map[string]bool{}} }

// Default is the registry vx records into and /metrics serves.
var Default = NewRegistry()

type metric interface {
	name() string
	write(w io.Writer)
}

func (r *Registry) add(m metric) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names[m.name()] {
		panic("metrics: duplicate metric " + m.name())
	}
	r.names[m.name()] = true
	r.metrics = append(r.metrics, m)
}

// WriteText writes every metric in the Prometheus text format, sorted by
// name.
func (r *Registry) WriteText(w io.Writer) {
	r.mu.Lock()
	ms := append([]metric(nil), r.metrics...)
	r.mu.Unlock()
	sort.Slice(ms, func(i, j int) bool { return ms[i].name() < ms[j].name() })
	for _, m := range ms {
		m.write(w)
	}
}

// ---------- labels ----------

type desc struct {
	n, help, kind string
	labels        []string
}

func (d desc) name() string { return d.n }

func (d desc) header(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", d.n, escapeHelp(d.help), d.n, d.kind)
}

// key joins label values; \xff never appears in them after cleaning.
func (d desc) key(values []string) string {
	if len(values) != len(d.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", d.n, len(d.labels), len(values)))
	}
	return strings.Join(values, "\xff")
}

// labelText renders {a="x",b="y"} for the values in key, plus extra
// (already rendered, like le="0.5").
func (d desc) labelText(key string, extra string) string {
	var parts []string
	if len(d.labels) > 0 {
		for i, v := range strings.Split(key, "\xff") {
			parts = append(parts, d.labels[i]+`="`+escapeLabel(v)+`"`)
		}
	}
	if extra != "" {
		parts = append(parts, extra)
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(s)
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// maxSeries bounds the label combinations of one metric, so a bug that
// puts an unbounded value in a label cannot exhaust memory. Further
// combinations are counted under the value "other" for every label.
const maxSeries = 2000

// ---------- counters and gauges ----------

// Vec is a counter or gauge with labels.
type Vec struct {
	desc
	mu     sync.Mutex
	series map[string]*atomicFloat
	order  []string
}

type atomicFloat struct{ bits atomic.Uint64 }

func (a *atomicFloat) add(v float64) {
	for {
		old := a.bits.Load()
		if a.bits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+v)) {
			return
		}
	}
}

func (a *atomicFloat) set(v float64) { a.bits.Store(math.Float64bits(v)) }
func (a *atomicFloat) get() float64  { return math.Float64frombits(a.bits.Load()) }

// NewCounter registers a counter in r. Counters only go up.
func (r *Registry) NewCounter(name, help string, labels ...string) *Vec {
	return r.newVec(name, help, "counter", labels)
}

// NewGauge registers a gauge in r.
func (r *Registry) NewGauge(name, help string, labels ...string) *Vec {
	return r.newVec(name, help, "gauge", labels)
}

func (r *Registry) newVec(name, help, kind string, labels []string) *Vec {
	v := &Vec{desc: desc{n: name, help: help, kind: kind, labels: labels}, series: map[string]*atomicFloat{}}
	r.add(v)
	return v
}

func (v *Vec) get(values []string) *atomicFloat {
	k := v.key(values)
	v.mu.Lock()
	defer v.mu.Unlock()
	s := v.series[k]
	if s == nil {
		if len(v.series) >= maxSeries {
			k = v.key(otherValues(len(v.labels)))
			if s = v.series[k]; s != nil {
				return s
			}
		}
		s = &atomicFloat{}
		v.series[k] = s
		v.order = append(v.order, k)
	}
	return s
}

func otherValues(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "other"
	}
	return out
}

// Inc adds one.
func (v *Vec) Inc(values ...string) { v.get(values).add(1) }

// Add adds n; counters must not go down.
func (v *Vec) Add(n float64, values ...string) {
	if v.kind == "counter" && n < 0 {
		panic("metrics: counter " + v.n + " cannot go down")
	}
	v.get(values).add(n)
}

// Set sets a gauge.
func (v *Vec) Set(n float64, values ...string) { v.get(values).set(n) }

// Value returns the current value (tests).
func (v *Vec) Value(values ...string) float64 {
	k := v.key(values)
	v.mu.Lock()
	defer v.mu.Unlock()
	if s := v.series[k]; s != nil {
		return s.get()
	}
	return 0
}

func (v *Vec) write(w io.Writer) {
	v.header(w)
	v.mu.Lock()
	keys := append([]string(nil), v.order...)
	vals := make([]float64, len(keys))
	for i, k := range keys {
		vals[i] = v.series[k].get()
	}
	v.mu.Unlock()
	if len(keys) == 0 && len(v.labels) == 0 {
		fmt.Fprintf(w, "%s 0\n", v.n)
		return
	}
	idx := make([]int, len(keys))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return keys[idx[a]] < keys[idx[b]] })
	for _, i := range idx {
		fmt.Fprintf(w, "%s%s %s\n", v.n, v.labelText(keys[i], ""), formatFloat(vals[i]))
	}
}

// ---------- histograms ----------

// DefaultBuckets suit request latencies in seconds.
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// Histogram counts observations into buckets, per label combination.
type Histogram struct {
	desc
	buckets []float64
	mu      sync.Mutex
	series  map[string]*histSeries
}

type histSeries struct {
	counts []uint64 // per bucket, not cumulative
	count  uint64
	sum    float64
}

// NewHistogram registers a histogram; nil buckets: DefaultBuckets.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labels ...string) *Histogram {
	if buckets == nil {
		buckets = DefaultBuckets
	}
	h := &Histogram{desc: desc{n: name, help: help, kind: "histogram", labels: labels}, buckets: buckets, series: map[string]*histSeries{}}
	r.add(h)
	return h
}

// Observe records one value.
func (h *Histogram) Observe(v float64, values ...string) {
	k := h.key(values)
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.series[k]
	if s == nil {
		if len(h.series) >= maxSeries {
			k = h.key(otherValues(len(h.labels)))
			s = h.series[k]
		}
		if s == nil {
			s = &histSeries{counts: make([]uint64, len(h.buckets))}
			h.series[k] = s
		}
	}
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
			break
		}
	}
	s.count++
	s.sum += v
}

// Count returns how many values were observed (tests).
func (h *Histogram) Count(values ...string) uint64 {
	k := h.key(values)
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.series[k]; s != nil {
		return s.count
	}
	return 0
}

func (h *Histogram) write(w io.Writer) {
	h.header(w)
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]string, 0, len(h.series))
	for k := range h.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := h.series[k]
		var cum uint64
		for i, b := range h.buckets {
			cum += s.counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.n, h.labelText(k, `le="`+formatFloat(b)+`"`), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.n, h.labelText(k, `le="+Inf"`), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.n, h.labelText(k, ""), formatFloat(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.n, h.labelText(k, ""), s.count)
	}
}

// ---------- values read when scraped ----------

// Sample is one value of a metric read at scrape time.
type Sample struct {
	Labels []string // values, in the order of the metric's labels
	Value  float64
}

type funcMetric struct {
	desc
	read func() []Sample
}

// SetGaugeFunc registers a gauge whose samples read is called for on every
// scrape (queue depths, pool statistics), or replaces the read function of
// one registered before: each server started in a process (tests start
// many) points it at its own database. read must be fast: scrapes wait for
// it.
func (r *Registry) SetGaugeFunc(name, help string, read func() []Sample, labels ...string) {
	r.setFunc(&funcMetric{desc: desc{n: name, help: help, kind: "gauge", labels: labels}, read: read})
}

// SetCounterFunc is SetGaugeFunc for a value that only goes up.
func (r *Registry) SetCounterFunc(name, help string, read func() []Sample, labels ...string) {
	r.setFunc(&funcMetric{desc: desc{n: name, help: help, kind: "counter", labels: labels}, read: read})
}

func (r *Registry) setFunc(f *funcMetric) {
	r.mu.Lock()
	for i, m := range r.metrics {
		if m.name() == f.n {
			if _, ok := m.(*funcMetric); !ok {
				r.mu.Unlock()
				panic("metrics: " + f.n + " is not a function metric")
			}
			r.metrics[i] = f
			r.mu.Unlock()
			return
		}
	}
	r.mu.Unlock()
	r.add(f)
}

func (f *funcMetric) write(w io.Writer) {
	samples := f.read()
	f.header(w)
	for _, s := range samples {
		fmt.Fprintf(w, "%s%s %s\n", f.n, f.labelText(f.key(s.Labels), ""), formatFloat(s.Value))
	}
}

// One is a single unlabeled sample, for SetGaugeFunc.
func One(v float64) []Sample { return []Sample{{Value: v}} }
