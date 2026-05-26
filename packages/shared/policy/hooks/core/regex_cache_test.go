package core

import (
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestCompilePattern_SharedInstance(t *testing.T) {
	r1, err := CompilePattern(`(?i)hello`, "")
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := CompilePattern(`(?i)hello`, "")
	if r1 != r2 {
		t.Fatal("cache miss: same pattern returned different *regexp.Regexp")
	}
	if !r1.MatchString("HELLO world") {
		t.Fatal("regex does not match as expected")
	}
}

func TestCompilePattern_FlagsDistinct(t *testing.T) {
	r1, _ := CompilePattern(`hello`, "i")
	r2, _ := CompilePattern(`hello`, "")
	if r1 == r2 {
		t.Fatal("cache collision: different flags should produce distinct entries")
	}
}

func TestCompilePattern_InvalidRegexRejected(t *testing.T) {
	_, err := CompilePattern(`[`, "")
	if err == nil || !strings.Contains(err.Error(), "error parsing regexp") {
		t.Fatalf("expected parse error, got: %v", err)
	}
}

func TestCompilePattern_ConcurrentAccess(t *testing.T) {
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = CompilePattern(`(?i)concurrent`, "") }()
	}
	wg.Wait()
}

func TestCompilePattern_UnsupportedFlagRejected(t *testing.T) {
	_, err := CompilePattern(`x`, "Q")
	if err == nil {
		t.Fatalf("expected error for unsupported flag, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported regex flag") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCompilePattern_FlagCanonicalization(t *testing.T) {
	// Same logical flags in different order / duplicated → same cache entry.
	r1, _ := CompilePattern(`test`, "is")
	r2, _ := CompilePattern(`test`, "si")
	r3, _ := CompilePattern(`test`, "ssi")
	if r1 != r2 || r1 != r3 {
		t.Fatalf("flag canonicalization mismatch: %p / %p / %p", r1, r2, r3)
	}
}

func TestRegisterRegexCacheMetrics_Idempotent(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterRegexCacheMetrics(reg)
	// Second registration on the same registerer must NOT panic.
	RegisterRegexCacheMetrics(reg)
}

func TestRegisterRegexCacheMetrics_CountsHitsAndMisses(t *testing.T) {
	SetRegexCacheCap(10000) // fresh cache; note we cannot reset counters across test runs
	before := float64Value(t, regexCacheHits) + float64Value(t, regexCacheMisses)

	_, _ = CompilePattern(`(?i)counter-test-new`, "")
	_, _ = CompilePattern(`(?i)counter-test-new`, "")

	after := float64Value(t, regexCacheHits) + float64Value(t, regexCacheMisses)
	if delta := after - before; delta < 2 {
		t.Fatalf("expected at least 2 counter increments (1 miss + 1 hit), got %v", delta)
	}
}

// float64Value extracts the current value of a Prometheus counter using the
// dto Metric layer. Intended only for test assertions.
func float64Value(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("reading counter: %v", err)
	}
	return m.Counter.GetValue()
}
