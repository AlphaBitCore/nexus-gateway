package freshness

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// mustLoadSeedRulesJSONForBench mirrors loadSeedRulesJSON but accepts a
// testing.TB so benchmark code paths can call it.
func mustLoadSeedRulesJSONForBench(tb testing.TB) []Rule {
	tb.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		tb.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "..")
	path := filepath.Join(repoRoot, "tools", "db-migrate", "seed", "data", "time-sensitive-rules.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read seed rules JSON %s: %v", path, err)
	}
	var blob struct {
		Rules []Rule `json:"rules"`
	}
	if err := json.Unmarshal(raw, &blob); err != nil {
		tb.Fatalf("unmarshal seed rules JSON: %v", err)
	}
	return blob.Rules
}

// buildBenchmarkDetector builds a Detector with exactly n rules for use in
// benchmarks. The extra rules are padding rules that do NOT match the benchmark
// prompt, so the benchmark measures worst-case (no early exit) performance.
func buildBenchmarkDetector(tb testing.TB, n int) *Detector {
	tb.Helper()
	// Load real seed rules from the canonical JSON file so benchmark numbers
	// reflect the actual rule shape that ships. *testing.B does not have
	// Fatalf in the same nuance as *testing.T but both implement t.Helper()
	// and t.Fatalf through testing.TB.
	t, ok := tb.(*testing.T)
	var seed []Rule
	if ok {
		seed = loadSeedRulesJSON(t)
	} else {
		// Benchmark context — load via the same path-resolution logic.
		seed = mustLoadSeedRulesJSONForBench(tb)
	}
	rules := make([]Rule, 0, n)
	for i, r := range seed {
		if i >= n {
			break
		}
		rules = append(rules, r)
	}
	// Pad with no-match sentinel rules until we have exactly n rules.
	for i := len(rules); i < n; i++ {
		rules = append(rules, Rule{
			ID:                  fmt.Sprintf("padding-rule-%d", i),
			Keywords:            []string{fmt.Sprintf("__unique_keyword_%d__", i)},
			RequireQuestionMark: true,
			RequireEntity:       false,
			Languages:           []string{"en"},
			Enabled:             true,
		})
	}
	reg := prometheus.NewRegistry()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	d, err := NewDetector(rules, log, "nexus_bench", reg)
	if err != nil {
		tb.Fatalf("buildBenchmarkDetector: %v", err)
	}
	return d
}

// benchmarkPrompt is a ~100 character prompt that does NOT match any seed rule
// so the benchmark measures the full scan (no early exit).
const benchmarkPrompt = "Explain the concept of dependency injection in software architecture. How does it improve testability?"

// BenchmarkDetector_IsTimeSensitive_50Rules_200Words benchmarks 50-rule detection
// on a ~100-character no-match prompt. The SDD latency budget is ≤2ms p99;
// this benchmark must stay well under that to accommodate real-world overhead.
func BenchmarkDetector_IsTimeSensitive_50Rules_200Words(b *testing.B) {
	d := buildBenchmarkDetector(b, 50)
	prompt := buildPrompt(200)
	m := []ChatMessage{{Role: "user", Content: prompt}}
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		d.IsTimeSensitive(m)
	}
}

// BenchmarkDetector_IsTimeSensitive_200Rules benchmarks 200-rule detection on
// a ~100-character no-match prompt. Verifies that the upper rule-count bound
// from NFR-2 still stays within budget.
func BenchmarkDetector_IsTimeSensitive_200Rules(b *testing.B) {
	d := buildBenchmarkDetector(b, 200)
	m := msgs(benchmarkPrompt)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		d.IsTimeSensitive(m)
	}
}

// BenchmarkDetector_IsTimeSensitive_EarlyMatch benchmarks the early-exit path
// where the first rule matches immediately (best-case performance).
func BenchmarkDetector_IsTimeSensitive_EarlyMatch(b *testing.B) {
	d := buildBenchmarkDetector(b, 50)
	// Prompt that matches the "time-current" rule (first seed rule).
	m := msgs("What is happening today?")
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		d.IsTimeSensitive(m)
	}
}

// buildPrompt generates a no-match prompt of approximately wordCount words
// that does not contain any seed-rule keywords, to ensure the full rule list
// is scanned during a worst-case benchmark run.
func buildPrompt(wordCount int) string {
	words := []string{
		"explain", "concept", "dependency", "injection", "software", "architecture",
		"improves", "testability", "pattern", "interface", "module", "component",
		"system", "design", "principle", "abstraction", "coupling", "service",
		"object", "instance", "class", "method", "function", "parameter",
	}
	var sb strings.Builder
	for i := range wordCount {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(words[i%len(words)])
	}
	sb.WriteByte('.')
	return sb.String()
}
