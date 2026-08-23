package strategies

import (
	"context"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// statsFor builds a LatencyStatsFunc from a provider→(p95,total) map. A provider
// absent from the map reads cold (-1, 0), matching the tracker's contract.
func statsFor(m map[string][2]int) core.LatencyStatsFunc {
	return func(providerID string) (int, int) {
		if v, ok := m[providerID]; ok {
			return v[0], v[1]
		}
		return -1, 0
	}
}

// seededLatency builds a LatencyStrategy with a deterministic rng so shuffles and
// the ε-greedy roll are reproducible across test runs.
func seededLatency(stats core.LatencyStatsFunc) *LatencyStrategy {
	return &LatencyStrategy{
		lookup:       mockLookup,
		latencyStats: stats,
		rng:          rand.New(rand.NewPCG(1, 2)),
	}
}

func latencyNode(providers ...string) core.StrategyNode {
	targets := make([]core.LatencyTarget, len(providers))
	for i, p := range providers {
		targets[i] = core.LatencyTarget{ProviderID: p, ModelID: "m"}
	}
	return core.StrategyNode{Type: "latency", LatencyTargets: targets}
}

func evalLatency(t *testing.T, s *LatencyStrategy, node core.StrategyNode) ([]core.RoutingTarget, []core.TraceEntry) {
	t.Helper()
	var trace []core.TraceEntry
	got, err := s.Evaluate(context.Background(), node, &core.RoutingContext{}, &trace)
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	return got, trace
}

func providerOrder(targets []core.RoutingTarget) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = t.ProviderID
	}
	return out
}

func TestLatencyStrategy_Empty(t *testing.T) {
	s := seededLatency(statsFor(nil))
	got, trace := evalLatency(t, s, core.StrategyNode{Type: "latency"})
	if len(got) != 0 {
		t.Fatalf("expected no targets, got %v", providerOrder(got))
	}
	if len(trace) == 0 || !strings.Contains(trace[len(trace)-1].Decision, "no latencyTargets") {
		t.Fatalf("expected an empty-config trace, got %+v", trace)
	}
}

func TestLatencyStrategy_AllLookupsFail(t *testing.T) {
	failEvery := func(context.Context, string, string) (*core.RoutingTarget, error) { return nil, errStub }
	s := &LatencyStrategy{lookup: failEvery, latencyStats: statsFor(nil), rng: rand.New(rand.NewPCG(1, 2))}
	got, trace := evalLatency(t, s, latencyNode("a", "b"))
	if len(got) != 0 {
		t.Fatalf("expected no targets when every lookup fails, got %v", providerOrder(got))
	}
	if trace[len(trace)-1].Decision != "no targets resolved — returning no targets" {
		t.Fatalf("expected no-targets-resolved trace, got %q", trace[len(trace)-1].Decision)
	}
}

func TestLatencyStrategy_SoftDropOneLookup(t *testing.T) {
	// "bad" fails to resolve; the two good providers survive. selectiveLookup
	// resolves everything except "bad".
	s := &LatencyStrategy{
		lookup: func(ctx context.Context, providerID, modelID string) (*core.RoutingTarget, error) {
			if providerID == "bad" {
				return nil, errStub
			}
			return mockLookup(ctx, providerID, modelID)
		},
		latencyStats: statsFor(map[string][2]int{"fast": {50, 40}, "slow": {800, 40}}),
		rng:          rand.New(rand.NewPCG(1, 2)),
	}
	got, _ := evalLatency(t, s, latencyNode("fast", "bad", "slow"))
	order := providerOrder(got)
	if len(order) != 2 {
		t.Fatalf("expected the two resolvable targets, got %v", order)
	}
	// fast (bucket 0) must precede slow (bucket 8); bad is dropped.
	if order[0] != "fast" || order[1] != "slow" {
		t.Fatalf("expected [fast slow], got %v", order)
	}
}

func TestLatencyStrategy_WarmOrdersByP95(t *testing.T) {
	// AC-1: all warm, distinct buckets → fastest first, slowest last (deterministic:
	// each bucket has one member so the fastest-bucket shuffle is a no-op).
	s := seededLatency(statsFor(map[string][2]int{
		"mid":  {350, 30}, // bucket 3
		"fast": {40, 30},  // bucket 0
		"slow": {900, 30}, // bucket 9
	}))
	got, _ := evalLatency(t, s, latencyNode("mid", "fast", "slow"))
	if order := providerOrder(got); order[0] != "fast" || order[2] != "slow" {
		t.Fatalf("expected fastest first / slowest last, got %v", order)
	}
}

func TestLatencyStrategy_SameBucketKeepsConfigOrder(t *testing.T) {
	// AC-2: two warm targets in the same NON-fastest bucket keep config order
	// (quantization: sub-bucket jitter does not reorder them). "fast" alone holds
	// the fastest bucket so no shuffle perturbs the tail.
	s := seededLatency(statsFor(map[string][2]int{
		"fast": {30, 30},  // bucket 0 (fastest, single member → no shuffle)
		"b1":   {110, 30}, // bucket 1
		"b2":   {150, 30}, // bucket 1 (same bucket as b1)
	}))
	got, _ := evalLatency(t, s, latencyNode("fast", "b1", "b2"))
	if order := providerOrder(got); order[0] != "fast" || order[1] != "b1" || order[2] != "b2" {
		t.Fatalf("expected [fast b1 b2] (same-bucket stable), got %v", order)
	}
}

func TestLatencyStrategy_AllFailureSortsLast(t *testing.T) {
	// A warm target with samples but zero successes (p95 == -1) sorts behind a
	// warm target with real latency data.
	s := seededLatency(statsFor(map[string][2]int{
		"broken": {-1, 40}, // warm-eligible, all failures
		"ok":     {500, 40},
	}))
	got, _ := evalLatency(t, s, latencyNode("broken", "ok"))
	if order := providerOrder(got); order[0] != "ok" || order[1] != "broken" {
		t.Fatalf("expected [ok broken] (all-failure last), got %v", order)
	}
}

func TestLatencyStrategy_AllCold(t *testing.T) {
	// No target reaches latencyMinSamples → every target is returned (random
	// load-balance), set preserved.
	s := seededLatency(statsFor(map[string][2]int{
		"a": {50, 5}, "b": {60, 5}, "c": {70, 5},
	}))
	got, _ := evalLatency(t, s, latencyNode("a", "b", "c"))
	if len(got) != 3 {
		t.Fatalf("expected all 3 cold targets returned, got %v", providerOrder(got))
	}
	assertSameSet(t, providerOrder(got), []string{"a", "b", "c"})
}

func TestLatencyStrategy_NilStatsIsAllCold(t *testing.T) {
	// A nil stats func (no tracker wired) must not strand the request: every
	// target reads cold and all are returned.
	s := &LatencyStrategy{lookup: mockLookup, latencyStats: nil, rng: rand.New(rand.NewPCG(3, 4))}
	got, _ := evalLatency(t, s, latencyNode("a", "b"))
	assertSameSet(t, providerOrder(got), []string{"a", "b"})
}

func TestLatencyStrategy_MixedExploit(t *testing.T) {
	// AC-3 exploit branch: warm fastest served at position 0, cold in the tail.
	s := seededLatency(statsFor(map[string][2]int{
		"warmfast": {40, 40}, // warm bucket 0
		"warmslow": {600, 40},
		// "cold" absent → cold
	}))
	stats := []latencyStat{
		{target: mustResolve(t, "warmfast"), p95Ms: 40, total: 40},
		{target: mustResolve(t, "warmslow"), p95Ms: 600, total: 40},
		{target: mustResolve(t, "cold"), p95Ms: -1, total: 0},
	}
	ordered, mode := s.order(stats, false)
	got := providerOrder(ordered)
	if mode != "exploit" {
		t.Fatalf("expected exploit mode, got %q", mode)
	}
	if got[0] != "warmfast" {
		t.Fatalf("exploit: expected warm fastest served first, got %v", got)
	}
	if got[len(got)-1] != "cold" {
		t.Fatalf("exploit: expected cold in the failover tail, got %v", got)
	}
}

func TestLatencyStrategy_MixedExplore(t *testing.T) {
	// AC-3 explore branch: a cold target takes the served position.
	s := seededLatency(nil)
	stats := []latencyStat{
		{target: mustResolve(t, "warmfast"), p95Ms: 40, total: 40},
		{target: mustResolve(t, "cold"), p95Ms: -1, total: 0},
	}
	ordered, mode := s.order(stats, true)
	got := providerOrder(ordered)
	if mode != "explore" {
		t.Fatalf("expected explore mode, got %q", mode)
	}
	if got[0] != "cold" {
		t.Fatalf("explore: expected cold target served first, got %v", got)
	}
	assertSameSet(t, got, []string{"warmfast", "cold"})
}

func TestLatencyStrategy_FastestBucketShuffleSpreadsLoad(t *testing.T) {
	// Two warm targets share the fastest bucket; both must precede the slower one,
	// and across seeds the served position must be able to vary (load spread).
	stats := map[string][2]int{
		"f1": {30, 40}, "f2": {60, 40}, // both bucket 0
		"slow": {700, 40}, // bucket 7
	}
	seen := map[string]bool{}
	for seed := uint64(1); seed <= 20; seed++ {
		s := &LatencyStrategy{lookup: mockLookup, latencyStats: statsFor(stats), rng: rand.New(rand.NewPCG(seed, seed+1))}
		got, _ := evalLatency(t, s, latencyNode("f1", "f2", "slow"))
		order := providerOrder(got)
		if order[2] != "slow" {
			t.Fatalf("slow target must stay last, got %v", order)
		}
		if order[0] != "f1" && order[0] != "f2" {
			t.Fatalf("a fastest-bucket target must be served first, got %v", order)
		}
		seen[order[0]] = true
	}
	if len(seen) < 2 {
		t.Fatalf("fastest-bucket shuffle never varied the served target across 20 seeds: %v", seen)
	}
}

func TestLatencyStrategy_ExploreProbabilityBounded(t *testing.T) {
	// explore() must fire at roughly latencyExploreProb. Wide bounds keep it
	// non-flaky while still asserting the real probability (not a constant).
	s := seededLatency(nil)
	hits := 0
	const n = 20000
	for range n {
		if s.explore() {
			hits++
		}
	}
	frac := float64(hits) / float64(n)
	if frac < 0.05 || frac > 0.15 {
		t.Fatalf("explore() fraction %.3f outside [0.05,0.15] for target %.2f", frac, latencyExploreProb)
	}
}

func TestLatencyStrategy_TraceDescribesEachTarget(t *testing.T) {
	s := seededLatency(statsFor(map[string][2]int{"fast": {40, 40}}))
	_, trace := evalLatency(t, s, latencyNode("fast", "coldy"))
	dec := trace[len(trace)-1].Decision
	if !strings.Contains(dec, "warm") || !strings.Contains(dec, "cold") || !strings.Contains(dec, "p95=40ms") {
		t.Fatalf("trace should describe warm/cold + p95, got %q", dec)
	}
	if !strings.Contains(dec, "bucket=0") {
		t.Fatalf("trace should describe the latency bucket, got %q", dec)
	}
	// The mode label (all-cold/warm-only/explore/exploit) must be present so an
	// operator can see which branch drove the decision.
	if !strings.Contains(dec, "latency-ranked(") {
		t.Fatalf("trace should carry the decision mode, got %q", dec)
	}
}

func TestLatencyStrategy_RegisteredAndDispatches(t *testing.T) {
	// Integration: RegisterAllStrategies registers "latency" and the registry
	// dispatches to it. A nil stats func is passed (all-cold), so both resolve.
	reg := NewStrategyRegistry()
	RegisterAllStrategies(reg, mockLookup, nil, nil)
	var trace []core.TraceEntry
	got, err := reg.Evaluate(context.Background(), latencyNode("a", "b"), &core.RoutingContext{}, &trace)
	if err != nil {
		t.Fatalf("registry Evaluate error: %v", err)
	}
	assertSameSet(t, providerOrder(got), []string{"a", "b"})
}

func TestLatencyStrategy_NilRngUsesGlobal(t *testing.T) {
	// A nil rng must fall back to the global source without panicking and still
	// return the full set.
	s := &LatencyStrategy{lookup: mockLookup, latencyStats: statsFor(map[string][2]int{"a": {10, 5}, "b": {20, 5}})}
	got, _ := evalLatency(t, s, latencyNode("a", "b"))
	assertSameSet(t, providerOrder(got), []string{"a", "b"})
}

func mustResolve(t *testing.T, provider string) core.RoutingTarget {
	t.Helper()
	target, err := mockLookup(context.Background(), provider, "m")
	if err != nil {
		t.Fatalf("mockLookup(%s): %v", provider, err)
	}
	return *target
}

func assertSameSet(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("set size mismatch: got %v want %v", got, want)
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			t.Fatalf("missing %q in %v", w, got)
		}
		seen[w]--
	}
}
