package geminicache

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds the Prometheus instruments for the Gemini cachedContent subsystem.
type Metrics struct {
	hit       *prometheus.CounterVec
	miss      *prometheus.CounterVec
	createOK  *prometheus.CounterVec
	createErr *prometheus.CounterVec
	skipped   *prometheus.CounterVec
}

// NewMetrics registers and returns the Gemini cache instruments under the
// given Prometheus registerer. Pass prometheus.DefaultRegisterer in
// production; pass a fresh prometheus.NewRegistry() in tests.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	factory := func(name, help string, labels ...string) *prometheus.CounterVec {
		cv := prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nexus_aigw_gemini_cache_" + name + "_total",
			Help: help,
		}, labels)
		reg.MustRegister(cv)
		return cv
	}
	return &Metrics{
		hit:       factory("hit", "Gemini cachedContent cache hits (Redis key present).", "model"),
		miss:      factory("miss", "Gemini cachedContent cache misses (Redis key absent).", "model"),
		createOK:  factory("create_ok", "Successful Gemini cachedContent creations.", "model"),
		createErr: factory("create_err", "Failed Gemini cachedContent creation attempts.", "model"),
		skipped:   factory("skipped", "Requests skipped by geminicache (below threshold, disabled, circuit open).", "reason"),
	}
}

func (m *Metrics) recordHit(model string)      { m.hit.WithLabelValues(model).Inc() }
func (m *Metrics) recordMiss(model string)     { m.miss.WithLabelValues(model).Inc() }
func (m *Metrics) recordCreateOK(model string) { m.createOK.WithLabelValues(model).Inc() }
func (m *Metrics) recordCreateErr(model string) {
	m.createErr.WithLabelValues(model).Inc()
}
func (m *Metrics) recordSkipped(reason string) { m.skipped.WithLabelValues(reason).Inc() }
