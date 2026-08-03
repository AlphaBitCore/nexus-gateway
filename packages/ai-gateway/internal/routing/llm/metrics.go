package llm

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// InputOverflowTotal counts router-LLM calls whose conversation input
	// exceeded the computed budget (label = the inputstaging overflow
	// kind) or whose system prompt consumed the router window (label =
	// "system_prompt_budget_floor"). The pipeline fails open — budget
	// enforcement bounds the content and the call proceeds — so this
	// counter surfaces overflow frequency for operator alerting, e.g. a
	// catalog outgrowing the configured router model.
	InputOverflowTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nexus", Subsystem: "smart_router",
		Name: "input_overflow_total", Help: "Router-LLM calls where the conversation input overflowed the computed budget.",
	}, []string{"overflow_kind"})
)

// Register wires all smart-router metrics into the provided registerer.
// Idempotent: AlreadyRegisteredError is treated as no-op.
func Register(reg prometheus.Registerer) {
	collectors := []prometheus.Collector{
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
