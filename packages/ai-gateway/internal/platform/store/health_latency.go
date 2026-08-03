package store

import (
	"sort"
	"time"
)

// GetLatencyP95 returns the 95th-percentile latency (nearest-rank) over the
// provider's SUCCESSFUL in-window samples, plus the TOTAL in-window sample count
// (success + failure). It reads the same immutable snapshot lock-free as
// GetHealth and applies the same 5-min read-time cutoff; it never mutates the
// shared snapshot.
//
// p95 is success-only on purpose: a fast 4xx or a slow timeout must not move the
// latency ranking (error behaviour is owned by the health band, not the latency
// signal). The TOTAL count is returned separately so the latency strategy's
// warm/cold gate counts every attempt — a target under active use warms up even
// when some of its attempts fail, so it cannot stay permanently "cold" at a low
// success rate.
//
// Return contract:
//   - total == 0: no in-window sample at all → the strategy treats the target as
//     cold (never attempted, or all samples aged out).
//   - p95Ms == -1 with total > 0: the target has samples but none succeeded in
//     the window (all failures) → the strategy sorts it last among warm targets;
//     the health band demotes it regardless.
//   - otherwise p95Ms is the nearest-rank p95 over the successful samples.
func (ht *HealthTracker) GetLatencyP95(providerID string) (p95Ms int, total int) {
	s := ht.snap.Load()
	w := s.windows[providerID]
	if w == nil {
		return -1, 0
	}

	cutoff := time.Now().Add(-healthWindowDuration)
	latencies := make([]int, 0, len(w.samples))
	for i := range w.samples {
		sm := &w.samples[i]
		if sm.timestamp.Before(cutoff) {
			continue
		}
		total++
		if sm.success {
			latencies = append(latencies, sm.latencyMs)
		}
	}

	if total == 0 {
		return -1, 0
	}
	if len(latencies) == 0 {
		return -1, total // warm-eligible but no successful sample
	}

	sort.Ints(latencies)
	// Nearest-rank p95: index ceil(0.95*n) - 1 (n=1→0, n=20→18, n=100→94).
	rank := (95*len(latencies) + 99) / 100 // ceil(0.95*n)
	return latencies[rank-1], total
}
