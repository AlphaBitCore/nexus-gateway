//go:build vectorscan

package matcher

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

// TestProfilePerRuleScan is an opt-in diagnostic (set NEXUS_PERF_PROFILE=1) that
// times every seed rule compiled ALONE against a 50KB benign corpus and prints a
// descending ranking. A single pathological rule (a wide literal-less repeat that
// defeats the Teddy/FDR prefilter) inflates the combined per-hook database and
// slows EVERY rule in it — this is the empirical complement to the static
// no_literal_prefilter lint, and how the 246us email outlier was located. It is
// env-gated because wall-clock timing would be flaky under CI parallelism; the
// number to watch is the slowest rule, which sits at the ~50KB memory-bandwidth
// floor (~20-28us, ~1.8 GB/s) once no outlier remains.
func TestProfilePerRuleScan(t *testing.T) {
	if os.Getenv("NEXUS_PERF_PROFILE") == "" {
		t.Skip("set NEXUS_PERF_PROFILE=1 to run the per-rule scan-cost profiler")
	}
	dir := filepath.Join("..", "..", "..", "..", "..", "tools", "db-migrate", "seed", "rule-packs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no seed dir: %v", err)
	}
	var para = "The quick brown fox jumps over the lazy dog while discussing quarterly revenue projections and supply-chain logistics in considerable, unremarkable detail. "
	var sb strings.Builder
	for sb.Len() < 50000 {
		sb.WriteString(para)
	}
	corpus := []string{sb.String()}

	type ruleRef struct{ pack, id, expr, flags string }
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var rules []ruleRef
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		pack, _, err := rulepack.LoadYAML(data)
		if err != nil {
			continue
		}
		for _, r := range pack.Rules {
			rules = append(rules, ruleRef{strings.TrimSuffix(name, ".yaml"), r.RuleID, r.Pattern, r.Flags})
		}
	}

	type result struct {
		ruleRef
		us float64
	}
	var results []result
	for _, r := range rules {
		m, bad := CompileVectorscan([]Pattern{{ID: 0, Expr: r.expr, Flags: r.flags}})
		if len(bad) > 0 {
			m.(*vectorscanMatcher).Close()
			continue // RE2-residual: no Vectorscan scan cost on the hot path
		}
		for i := 0; i < 20; i++ {
			m.Scan(corpus, true)
		}
		const iters = 200
		start := time.Now()
		for i := 0; i < iters; i++ {
			m.Scan(corpus, true)
		}
		us := float64(time.Since(start).Microseconds()) / float64(iters)
		m.(*vectorscanMatcher).Close()
		results = append(results, result{r, us})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].us > results[j].us })
	fmt.Printf("\n=== per-rule Vectorscan scan cost, 50KB benign, desc (top 30 of %d) ===\n", len(results))
	for i, r := range results {
		if i >= 30 {
			break
		}
		expr := r.expr
		if len(expr) > 70 {
			expr = expr[:70] + "..."
		}
		fmt.Printf("%6.1fus  %-32s %s\n", r.us, r.pack+"/"+r.id, expr)
	}
}
