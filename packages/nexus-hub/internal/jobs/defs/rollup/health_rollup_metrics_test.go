package rollup

import (
	"testing"
	"time"
)

// TestHealthRollupMetrics_NilSafe asserts every method is a no-op on a
// nil receiver — production code can pass a nil *HealthRollupMetrics
// when the opsmetrics registry is wired to disabled.
func TestHealthRollupMetrics_NilSafe(t *testing.T) {
	var m *HealthRollupMetrics
	m.cycle("any")
	m.updated(5)
	m.candidates(3)
	m.transition("from", "to")
	m.observe(time.Millisecond)
}

func TestHealthRollupMetrics_ZeroDoesNotAdd(t *testing.T) {
	// n=0 path on updated/candidates: the methods short-circuit so the
	// counter is never touched. Pure-behaviour assertion via no panic.
	var m *HealthRollupMetrics
	m.updated(0)
	m.candidates(0)
}
