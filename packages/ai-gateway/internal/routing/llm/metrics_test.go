package llm

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// Register must be idempotent: wiring code may run it more than once
// (main + tests), and a duplicate registration must be a no-op rather
// than a panic — only a genuinely conflicting collector panics.
func TestRegister_Idempotent(t *testing.T) {
	reg := prometheus.NewRegistry()
	Register(reg)
	Register(reg) // second call must be a no-op, not a panic

	// The collector must actually be registered: incrementing and
	// gathering shows the family exists.
	InputOverflowTotal.WithLabelValues("single_message_too_big").Inc()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	found := false
	for _, f := range families {
		if f.GetName() == "nexus_smart_router_input_overflow_total" {
			found = true
		}
	}
	if !found {
		t.Errorf("nexus_smart_router_input_overflow_total not registered")
	}
}
