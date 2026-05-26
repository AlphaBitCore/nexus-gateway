package pipeline

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds the Prometheus metrics for the compliance pipeline.
// Create via RegisterMetrics(namespace).
type Metrics struct {
	PipelineDuration      prometheus.Histogram
	HookDuration          *prometheus.HistogramVec
	HookDecisionTotal     *prometheus.CounterVec
	PipelineDecisionTotal *prometheus.CounterVec
	HookErrorTotal        *prometheus.CounterVec
	HookTimeoutTotal      *prometheus.CounterVec
	// PipelineSkippedTotal counts hooks excluded at BuildPipeline time due to
	// endpoint or modality mismatch. Labels: endpoint, reason, stage.
	PipelineSkippedTotal *prometheus.CounterVec
}

// RegisterMetrics creates and registers compliance metrics under the given
// namespace (e.g. "nexus_agent", "nexus_compliance_proxy") on the supplied
// registerer. Pass nil to use prometheus.DefaultRegisterer.
//
// The first call also sets the package-level convenience vars (PipelineDuration
// etc.) so pipeline.go can reference them without a *Metrics argument.
// Subsequent calls register additional metric sets but do not update the
// convenience vars.
func RegisterMetrics(reg prometheus.Registerer, namespace string) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	factory := promauto.With(reg)
	m := &Metrics{
		PipelineDuration: factory.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: "compliance",
			Name:      "pipeline_duration_seconds",
			Help:      "Total compliance pipeline execution duration",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
		}),
		HookDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: "compliance",
			Name:      "hook_duration_seconds",
			Help:      "Per-hook execution duration",
			Buckets:   []float64{0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25},
		}, []string{"hook"}),
		HookDecisionTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "compliance",
			Name:      "hook_decision_total",
			Help:      "Total hook decisions by hook name and decision",
		}, []string{"hook", "decision"}),
		PipelineDecisionTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "compliance",
			Name:      "pipeline_decision_total",
			Help:      "Total pipeline decisions by decision type",
		}, []string{"decision"}),
		HookErrorTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "compliance",
			Name:      "hook_error_total",
			Help:      "Total hook execution errors by hook name",
		}, []string{"hook"}),
		HookTimeoutTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "compliance",
			Name:      "hook_timeout_total",
			Help:      "Total hook timeouts by hook name",
		}, []string{"hook"}),
		PipelineSkippedTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "compliance",
			Name:      "pipeline_skipped_total",
			Help:      "Total hooks excluded at BuildPipeline time due to endpoint or modality mismatch",
		}, []string{"endpoint", "reason", "stage"}),
	}
	metricsOnce.Do(func() {
		PipelineDuration = m.PipelineDuration
		PipelineDecisionTotal = m.PipelineDecisionTotal
		HookDuration = m.HookDuration
		HookErrorTotal = m.HookErrorTotal
		HookTimeoutTotal = m.HookTimeoutTotal
		HookDecisionTotal = m.HookDecisionTotal
		PipelineSkippedTotal = m.PipelineSkippedTotal
	})
	return m
}

// metricsOnce ensures the package-level convenience vars are set exactly once
// by the first call to RegisterMetrics.
var metricsOnce sync.Once

// noopRegistry is a separate Prometheus registry used for no-op metrics so
// that the default no-op vars do not collide with the real DefaultRegisterer
// when RegisterMetrics is eventually called.
var noopRegistry = prometheus.NewRegistry()

// noopFactory registers no-op metrics into an isolated registry.
var noopFactory = promauto.With(noopRegistry)

// Package-level convenience vars for pipeline.go. Initialised to no-op
// metrics so that pipelines running in tests (before RegisterMetrics is
// called) do not panic. The first call to RegisterMetrics replaces these with
// the real, namespace-scoped metrics.
var (
	PipelineDuration prometheus.Observer = noopFactory.NewHistogram(prometheus.HistogramOpts{
		Name: "noop_pipeline_duration_seconds",
		Help: "no-op; replaced by first RegisterMetrics call",
	})
	PipelineDecisionTotal *prometheus.CounterVec = noopFactory.NewCounterVec(prometheus.CounterOpts{
		Name: "noop_pipeline_decision_total",
		Help: "no-op; replaced by first RegisterMetrics call",
	}, []string{"decision"})
	HookDuration *prometheus.HistogramVec = noopFactory.NewHistogramVec(prometheus.HistogramOpts{
		Name: "noop_hook_duration_seconds",
		Help: "no-op; replaced by first RegisterMetrics call",
	}, []string{"hook"})
	HookErrorTotal *prometheus.CounterVec = noopFactory.NewCounterVec(prometheus.CounterOpts{
		Name: "noop_hook_error_total",
		Help: "no-op; replaced by first RegisterMetrics call",
	}, []string{"hook"})
	HookTimeoutTotal *prometheus.CounterVec = noopFactory.NewCounterVec(prometheus.CounterOpts{
		Name: "noop_hook_timeout_total",
		Help: "no-op; replaced by first RegisterMetrics call",
	}, []string{"hook"})
	HookDecisionTotal *prometheus.CounterVec = noopFactory.NewCounterVec(prometheus.CounterOpts{
		Name: "noop_hook_decision_total",
		Help: "no-op; replaced by first RegisterMetrics call",
	}, []string{"hook", "decision"})
	PipelineSkippedTotal *prometheus.CounterVec = noopFactory.NewCounterVec(prometheus.CounterOpts{
		Name: "noop_pipeline_skipped_total",
		Help: "no-op; replaced by first RegisterMetrics call",
	}, []string{"endpoint", "reason", "stage"})
)
