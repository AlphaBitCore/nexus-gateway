package aiguard

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	CacheHitsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "nexus", Subsystem: "aiguard",
		Name: "cache_hits_total", Help: "Number of classify calls served from cache.",
	})
	CacheMissesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "nexus", Subsystem: "aiguard",
		Name: "cache_misses_total", Help: "Number of classify calls that missed cache.",
	})
	CacheWritesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "nexus", Subsystem: "aiguard",
		Name: "cache_writes_total", Help: "Number of cache entries written.",
	})
	JudgeLatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "nexus", Subsystem: "aiguard",
		Name: "judge_latency_seconds", Help: "Judge backend latency.",
		Buckets: prometheus.DefBuckets,
	}, []string{"backend_mode"})
	JudgeErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus", Subsystem: "aiguard",
		Name: "judge_errors_total", Help: "Judge backend errors by kind.",
	}, []string{"backend_mode", "kind"})
	DecisionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus", Subsystem: "aiguard",
		Name: "decisions_total", Help: "Classify decisions by detector_type and decision.",
	}, []string{"detector_type", "decision"})
	// InputOverflowTotal counts classify calls where inputstaging.Plan
	// detected an overflow (conversation too long for the judge model's
	// context window). The pipeline fails-open — the truncated content is
	// forwarded to the judge regardless — so this counter surfaces the
	// frequency of overflow events for operator alerting.
	InputOverflowTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus", Subsystem: "aiguard",
		Name: "input_overflow_total", Help: "Classify calls where inputstaging detected a context overflow.",
	}, []string{"overflow_kind"})
)

// Register wires all ai-guard metrics into the provided registerer.
// Idempotent: AlreadyRegisteredError is treated as no-op.
func Register(reg prometheus.Registerer) {
	collectors := []prometheus.Collector{
		CacheHitsTotal, CacheMissesTotal, CacheWritesTotal,
		JudgeLatencySeconds, JudgeErrorsTotal, DecisionsTotal,
		InputOverflowTotal,
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			var already prometheus.AlreadyRegisteredError
			if !errors.As(err, &already) {
				panic(err)
			}
		}
	}
}
