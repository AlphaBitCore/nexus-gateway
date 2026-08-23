package strategies

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

const (
	// latencyMinSamples is the warm/cold gate: a target with fewer than this many
	// TOTAL in-window samples (success+failure) has too little data for a stable
	// p95 and is treated as cold (exploratory). Counted over total attempts so a
	// target under live use warms up even when some attempts fail.
	latencyMinSamples = 20

	// latencyBucketMs quantizes p95 into equal-speed tiers. Two providers whose
	// p95 differ by less than one bucket are treated as equally fast, so
	// sub-bucket jitter never reorders them — the anti-flap guard. Coarse enough
	// to kill flap, fine enough that a genuinely faster provider still wins.
	latencyBucketMs = 100

	// latencyExploreProb is the ε-greedy exploration share: in the mixed
	// (some-warm, some-cold) case, this fraction of requests serve a random cold
	// target so cold targets accumulate samples and warm up, while the remainder
	// exploit the measured-fastest warm target. Small enough to bound exploration
	// latency cost, large enough to warm a cold target at any non-trivial QPS.
	latencyExploreProb = 0.10
)

// LatencyStrategy orders its targets by observed p95 latency: the measured
// fastest provider is served first, with bounded exploration of cold (too-few
// samples) targets and load-spread across equally-fast providers. It reuses the
// health tracker's existing per-provider latency window (no new subsystem) and
// composes with the unchanged health-band reorder that runs afterward (band
// dominates, latency orders within a band).
type LatencyStrategy struct {
	lookup       core.TargetLookup
	latencyStats core.LatencyStatsFunc
	// rng is an optional seeded source for deterministic, single-goroutine tests.
	// Production leaves it nil so every draw uses the package-global math/rand/v2
	// source (concurrency-safe), exactly as loadbalance/ab_split do — Evaluate is
	// called concurrently (one per in-flight request). A math/rand/v2 *rand.Rand
	// is NOT safe for concurrent use, so a non-nil rng must only be set on a
	// strategy instance that a single goroutine drives (tests), never the shared
	// production singleton.
	rng *rand.Rand
}

func (s *LatencyStrategy) Type() string { return "latency" }

// latencyStat pairs a resolved target with its observed latency-window stats.
type latencyStat struct {
	target core.RoutingTarget
	p95Ms  int // -1 = no successful in-window sample (all-failure or cold)
	total  int // total in-window samples (success+failure)
}

func (s *LatencyStrategy) Evaluate(ctx context.Context, node core.StrategyNode, _ *core.RoutingContext, trace *[]core.TraceEntry) ([]core.RoutingTarget, error) {
	if len(node.LatencyTargets) == 0 {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "latency",
			Decision:     "no latencyTargets configured — returning no targets",
		})
		return nil, nil
	}

	stats := make([]latencyStat, 0, len(node.LatencyTargets))
	for _, lt := range node.LatencyTargets {
		target, err := s.lookup(ctx, lt.ProviderID, lt.ModelID)
		if err != nil {
			// Soft failure — drop this target and continue, matching single/ab_split.
			*trace = append(*trace, core.TraceEntry{
				StrategyType: "latency",
				Decision:     fmt.Sprintf("lookup failed: %s/%s: %v", lt.ProviderID, lt.ModelID, err),
			})
			continue
		}
		p95, total := -1, 0
		if s.latencyStats != nil {
			p95, total = s.latencyStats(lt.ProviderID)
		}
		stats = append(stats, latencyStat{target: *target, p95Ms: p95, total: total})
	}
	if len(stats) == 0 {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "latency",
			Decision:     "no targets resolved — returning no targets",
		})
		return nil, nil
	}

	ordered, mode := s.order(stats, s.explore())
	*trace = append(*trace, core.TraceEntry{
		StrategyType: "latency",
		Decision:     describeLatencyOrder(ordered, stats, mode),
	})
	return ordered, nil
}

// order partitions targets into warm/cold and produces the served order per §4.
// The explore flag (an ε-greedy roll made by the caller) only matters in the
// mixed case; it is a parameter so the two mixed branches are deterministically
// testable independent of the rng draw:
//   - all cold        → random spread (safe cold-start / no-tracker load-balance)
//   - no cold         → pure warm p95 order
//   - mixed + explore → a random cold target at the served position
//   - mixed + exploit → the warm fastest served, cold in the failover tail
//
// order returns the served target order and a mode label ("all-cold" /
// "warm-only" / "explore" / "exploit") describing which branch produced it, so
// the audit trace records what actually drove the decision.
func (s *LatencyStrategy) order(stats []latencyStat, explore bool) ([]core.RoutingTarget, string) {
	var warm, cold []latencyStat
	for _, st := range stats {
		if st.total >= latencyMinSamples {
			warm = append(warm, st)
		} else {
			cold = append(cold, st)
		}
	}

	coldTargets := s.shuffledTargets(cold)
	if len(warm) == 0 {
		return coldTargets, "all-cold" // random load-balance until samples accumulate
	}
	warmTargets := s.orderWarm(warm)
	if len(cold) == 0 {
		return warmTargets, "warm-only" // steady state: pure latency order
	}

	// Mixed: bounded ε-greedy so cold targets earn samples without monopolizing
	// the served position.
	if explore {
		out := make([]core.RoutingTarget, 0, len(warmTargets)+len(coldTargets))
		out = append(out, coldTargets[0])     // exploration pick at the served position
		out = append(out, warmTargets...)     // then the warm order
		out = append(out, coldTargets[1:]...) // remaining cold in the tail
		return out, "explore"
	}
	// Exploit: warm fastest served; cold targets in the failover tail.
	return append(warmTargets, coldTargets...), "exploit"
}

// orderWarm sorts warm targets by ascending latency bucket (stable within a
// bucket) and shuffles the fastest occupied bucket so served load spreads across
// equally-fast providers. Pure except for the fastest-bucket shuffle, which uses
// the rng seam.
func (s *LatencyStrategy) orderWarm(warm []latencyStat) []core.RoutingTarget {
	sort.SliceStable(warm, func(i, j int) bool {
		return latencyBucket(warm[i].p95Ms) < latencyBucket(warm[j].p95Ms)
	})
	if len(warm) > 1 {
		firstBucket := latencyBucket(warm[0].p95Ms)
		n := 0
		for n < len(warm) && latencyBucket(warm[n].p95Ms) == firstBucket {
			n++
		}
		if n > 1 {
			s.shuffle(warm[:n])
		}
	}
	out := make([]core.RoutingTarget, len(warm))
	for i, st := range warm {
		out[i] = st.target
	}
	return out
}

// latencyBucket quantizes a p95 into an equal-speed tier. A -1 p95 (warm target
// with no successful in-window sample) sorts last so a proven-broken target is
// never ranked ahead of one with real latency data.
func latencyBucket(p95Ms int) int {
	if p95Ms < 0 {
		return math.MaxInt
	}
	return p95Ms / latencyBucketMs
}

// shuffle randomises the given stats in place via the rng seam.
func (s *LatencyStrategy) shuffle(stats []latencyStat) {
	swap := func(i, j int) { stats[i], stats[j] = stats[j], stats[i] }
	if s.rng != nil {
		s.rng.Shuffle(len(stats), swap)
		return
	}
	rand.Shuffle(len(stats), swap)
}

// shuffledTargets shuffles a copy of the stats and returns just the targets.
func (s *LatencyStrategy) shuffledTargets(stats []latencyStat) []core.RoutingTarget {
	dup := make([]latencyStat, len(stats))
	copy(dup, stats)
	s.shuffle(dup)
	out := make([]core.RoutingTarget, len(dup))
	for i, st := range dup {
		out[i] = st.target
	}
	return out
}

// explore returns true with probability latencyExploreProb via the rng seam.
func (s *LatencyStrategy) explore() bool {
	if s.rng != nil {
		return s.rng.Float64() < latencyExploreProb
	}
	return rand.Float64() < latencyExploreProb
}

// describeLatencyOrder renders the SERVED order with each target's p95, latency
// bucket, sample count, and warm/cold state, prefixed by the decision mode, for
// the audit routing_trace — so an operator can retroactively see why a target
// was served (which one was fastest, whether it was an exploration pick).
func describeLatencyOrder(ordered []core.RoutingTarget, stats []latencyStat, mode string) string {
	byName := make(map[string]latencyStat, len(stats))
	for _, st := range stats {
		byName[st.target.ProviderName] = st
	}
	parts := make([]string, 0, len(ordered))
	for _, tgt := range ordered {
		st := byName[tgt.ProviderName]
		state := "cold"
		if st.total >= latencyMinSamples {
			state = "warm"
		}
		p95, bucket := "n/a", "n/a"
		if st.p95Ms >= 0 {
			p95 = fmt.Sprintf("%dms", st.p95Ms)
			bucket = fmt.Sprintf("%d", latencyBucket(st.p95Ms))
		}
		parts = append(parts, fmt.Sprintf("%s[%s p95=%s bucket=%s n=%d]", tgt.ProviderName, state, p95, bucket, st.total))
	}
	return "latency-ranked(" + mode + ") " + strings.Join(parts, " ")
}
