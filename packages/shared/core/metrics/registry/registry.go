package registry

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Registry holds opsmetrics instrument bindings and produces Sample batches
// via Collect(). Each instrument is also registered with an underlying
// Prometheus registerer so existing /metrics scrape paths keep working.
//
// "One registration site, two surfaces": every NewCounter / NewGauge /
// NewHistogram call creates a real Prometheus instrument (scraped by
// /metrics) AND records the binding so Collect() can return Samples for the
// Hub-bound sampler.
type Registry struct {
	prom      prometheus.Registerer
	namespace string

	mu       sync.RWMutex
	counters map[string]*Counter
	gauges   map[string]*Gauge
	histos   map[string]*Histogram
}

// HistogramBucketsMs is the canonical 6-bucket layout (spec §6.4).
// Five explicit upper bounds plus the implicit +Inf bucket = 6 total.
var HistogramBucketsMs = []float64{50, 100, 200, 500, 1000}

// DefaultNamespace is the Prometheus namespace prefix every opsmetrics
// instrument inherits. Mirrors the thingclient registry's "nexus" prefix
// so the entire Nexus Gateway scrape surface shares one root namespace.
// Tests that need raw names construct a Registry with an empty namespace
// (see NewRegistryWithNamespace).
const DefaultNamespace = "nexus"

// NewRegistry creates an opsmetrics Registry that also registers each
// instrument with the supplied Prometheus registerer. The default
// namespace ("nexus") is prepended to every metric name so the resulting
// Prometheus series share the gateway's root namespace.
func NewRegistry(prom prometheus.Registerer) *Registry {
	return NewRegistryWithNamespace(prom, DefaultNamespace)
}

// NewRegistryWithNamespace is the namespace-explicit constructor. Use
// `""` for an unprefixed registry; otherwise the supplied string is
// joined to the dotted metric name with `_`. Production callers should
// use NewRegistry (which pins "nexus"); the explicit form exists for
// tests that want raw names + for sub-systems that already prefix their
// instruments manually.
func NewRegistryWithNamespace(prom prometheus.Registerer, namespace string) *Registry {
	return &Registry{
		prom:      prom,
		namespace: namespace,
		counters:  map[string]*Counter{},
		gauges:    map[string]*Gauge{},
		histos:    map[string]*Histogram{},
	}
}

// promName converts a dotted opsmetrics name to a Prometheus snake_case
// series name, optionally prefixed by the registry namespace. Examples
// at namespace="nexus": cache.hits_total → nexus_cache_hits_total;
// diag.dropped_total → nexus_diag_dropped_total.
func (r *Registry) promName(dotted string) string {
	base := strings.ReplaceAll(dotted, ".", "_")
	if r.namespace == "" {
		return base
	}
	return r.namespace + "_" + base
}

// Counter wraps a Prometheus CounterVec and records cumulative values keyed by
// label combinations.
type Counter struct {
	name   string
	labels []string
	vec    *prometheus.CounterVec
}

// NewCounter returns the Counter for `name`, registering it on first call.
// Subsequent calls with the same name return the cached instance; the
// supplied labels are ignored on a cache hit (the original labels win).
func (r *Registry) NewCounter(name string, labels []string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	vec := prometheus.NewCounterVec(prometheus.CounterOpts{Name: r.promName(name)}, labels)
	if r.prom != nil {
		r.prom.MustRegister(vec)
	}
	c := &Counter{name: name, labels: labels, vec: vec}
	r.counters[name] = c
	return c
}

// With pins label values in declaration order. Callers MUST pass exactly
// len(labels) values; Prometheus will panic on arity mismatch when the pin
// is dereferenced (Inc/Add).
func (c *Counter) With(values ...string) *CounterPin {
	return &CounterPin{c: c, values: values}
}

// CounterPin is a Counter with its label values bound. The pin pattern makes
// it impossible to call Inc()/Add() without first declaring labels via With().
type CounterPin struct {
	c      *Counter
	values []string
}

func (p *CounterPin) Inc()          { p.c.vec.WithLabelValues(p.values...).Inc() }
func (p *CounterPin) Add(v float64) { p.c.vec.WithLabelValues(p.values...).Add(v) }

// Gauge wraps a Prometheus GaugeVec.
type Gauge struct {
	name   string
	labels []string
	vec    *prometheus.GaugeVec
}

// NewGauge returns the Gauge for `name`, registering it on first call.
func (r *Registry) NewGauge(name string, labels []string) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[name]; ok {
		return g
	}
	vec := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: r.promName(name)}, labels)
	if r.prom != nil {
		r.prom.MustRegister(vec)
	}
	g := &Gauge{name: name, labels: labels, vec: vec}
	r.gauges[name] = g
	return g
}

// With pins label values in declaration order. See Counter.With for arity rules.
func (g *Gauge) With(values ...string) *GaugePin {
	return &GaugePin{g: g, values: values}
}

// GaugePin is a Gauge with its label values bound.
type GaugePin struct {
	g      *Gauge
	values []string
}

func (p *GaugePin) Set(v float64) { p.g.vec.WithLabelValues(p.values...).Set(v) }
func (p *GaugePin) Inc()          { p.g.vec.WithLabelValues(p.values...).Inc() }
func (p *GaugePin) Dec()          { p.g.vec.WithLabelValues(p.values...).Dec() }
func (p *GaugePin) Add(v float64) { p.g.vec.WithLabelValues(p.values...).Add(v) }

// Histogram wraps a Prometheus HistogramVec with the canonical 6-bucket layout.
//
// We keep our own per-label-combination bucket counters (the [6]uint64 array)
// in addition to the Prometheus internal counters because the Prometheus Go
// client does not expose bucket counts via a public API; we want the same
// 6-element array shape in storage as defined by spec §6.4. The duplication
// is intentional.
type Histogram struct {
	name   string
	labels []string
	vec    *prometheus.HistogramVec

	mu      sync.Mutex
	buckets map[string]*[6]uint64 // keyed by joined label-values (NUL-separated)
}

// NewHistogram returns the Histogram for `name`, registering it on first call.
func (r *Registry) NewHistogram(name string, labels []string) *Histogram {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.histos[name]; ok {
		return h
	}
	vec := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    r.promName(name),
		Buckets: HistogramBucketsMs,
	}, labels)
	if r.prom != nil {
		r.prom.MustRegister(vec)
	}
	h := &Histogram{
		name: name, labels: labels, vec: vec,
		buckets: map[string]*[6]uint64{},
	}
	r.histos[name] = h
	return h
}

// With pins label values in declaration order. See Counter.With for arity rules.
func (h *Histogram) With(values ...string) *HistogramPin {
	return &HistogramPin{h: h, values: values, key: strings.Join(values, "\x00")}
}

// HistogramPin is a Histogram with its label values bound.
type HistogramPin struct {
	h      *Histogram
	values []string
	key    string
}

// Observe records a sample in both the Prometheus instrument and the
// internal bucket array.
func (p *HistogramPin) Observe(ms float64) {
	p.h.vec.WithLabelValues(p.values...).Observe(ms)
	idx := bucketIndex(ms)
	p.h.mu.Lock()
	bs, ok := p.h.buckets[p.key]
	if !ok {
		bs = &[6]uint64{}
		p.h.buckets[p.key] = bs
	}
	p.h.mu.Unlock()
	atomic.AddUint64(&bs[idx], 1)
}

// bucketIndex maps an observation in milliseconds to its bucket index per
// spec §6.4: [0,50) [50,100) [100,200) [200,500) [500,1000) [1000,+inf).
func bucketIndex(ms float64) int {
	switch {
	case ms < 50:
		return 0
	case ms < 100:
		return 1
	case ms < 200:
		return 2
	case ms < 500:
		return 3
	case ms < 1000:
		return 4
	default:
		return 5
	}
}

// Collect snapshots all instruments into Sample objects, one per
// (metric, dimension_key) pair.
func (r *Registry) Collect() []Sample {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []Sample
	for _, c := range r.counters {
		out = append(out, collectCounter(c)...)
	}
	for _, g := range r.gauges {
		out = append(out, collectGauge(g)...)
	}
	for _, h := range r.histos {
		out = append(out, collectHistogram(h)...)
	}
	return out
}

func collectCounter(c *Counter) []Sample {
	var samples []Sample
	ch := make(chan prometheus.Metric, 64)
	go func() { c.vec.Collect(ch); close(ch) }()
	for m := range ch {
		_, value, dim := decodeMetric(m, c.labels)
		samples = append(samples, Sample{
			Name:         c.name,
			Kind:         KindCounter,
			DimensionKey: dim,
			Value:        value,
		})
	}
	return samples
}

func collectGauge(g *Gauge) []Sample {
	var samples []Sample
	ch := make(chan prometheus.Metric, 64)
	go func() { g.vec.Collect(ch); close(ch) }()
	for m := range ch {
		_, value, dim := decodeMetric(m, g.labels)
		samples = append(samples, Sample{
			Name:         g.name,
			Kind:         KindGauge,
			DimensionKey: dim,
			Value:        value,
		})
	}
	return samples
}

func collectHistogram(h *Histogram) []Sample {
	h.mu.Lock()
	defer h.mu.Unlock()

	var samples []Sample
	for key, bs := range h.buckets {
		// Reconstruct dimension_key from the joined key.
		values := strings.Split(key, "\x00")
		dim := joinDimension(h.labels, values)
		buckets := make([]int, 6)
		for i := range 6 {
			buckets[i] = int(atomic.LoadUint64(&bs[i]))
		}
		samples = append(samples, Sample{
			Name:         h.name,
			Kind:         KindHistogram,
			DimensionKey: dim,
			Metadata:     map[string]any{"buckets": buckets},
		})
	}
	return samples
}

// decodeMetric extracts (placeholder, value, dimension_key) from a
// prometheus.Metric. Counter and Gauge values are obtained via the dto.Metric
// protobuf. The first return value is reserved for future per-metric DTO
// inspection by callers; callers may ignore it today.
func decodeMetric(m prometheus.Metric, labels []string) (placeholder string, value float64, dim string) {
	pb := &dtoMetric{}
	// Write returning err implies a metric in an unrecoverable state; we
	// report it as zero-valued and continue rather than aborting Collect.
	_ = m.Write(pb.proto())
	values := make([]string, len(labels))
	for i, name := range labels {
		values[i] = pb.label(name)
	}
	return "", pb.value(), joinDimension(labels, values)
}

// joinDimension renders labels+values as a stable, sorted "k=v;k=v" string
// to use as the Sample.DimensionKey.
func joinDimension(labels, values []string) string {
	if len(labels) == 0 {
		return ""
	}
	pairs := make([]string, len(labels))
	for i, l := range labels {
		pairs[i] = l + "=" + values[i]
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ";")
}

// dtoMetric is a thin wrapper around the Prometheus protobuf Metric type
// that exposes only the fields we need (counter/gauge value, label values).
type dtoMetric struct{ pb dto.Metric }

func (d *dtoMetric) proto() *dto.Metric { return &d.pb }

func (d *dtoMetric) value() float64 {
	if d.pb.Counter != nil {
		return d.pb.Counter.GetValue()
	}
	if d.pb.Gauge != nil {
		return d.pb.Gauge.GetValue()
	}
	return 0
}

func (d *dtoMetric) label(name string) string {
	for _, lp := range d.pb.Label {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}
