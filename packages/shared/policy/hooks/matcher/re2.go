package matcher

import (
	"regexp"
	"runtime"
	"sync"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// re2Matcher is the pure-Go implementation. Each pattern is compiled to its own
// `regexp.Regexp` via core.CompilePattern — the SAME compilation the rule-pack
// engine uses today, so RE2 match results are byte-identical to the current
// engine (the differential-test invariant).
type re2Matcher struct {
	pats []re2Pat
}

type re2Pat struct {
	id int
	re *regexp.Regexp
}

// CompileRE2 builds an RE2 matcher. Patterns that fail to compile are skipped
// and returned in `bad`; the returned Matcher contains every pattern that did
// compile. Mirrors the engine's per-rule skip+log fail-posture.
func CompileRE2(pats []Pattern) (Matcher, []BadPattern) {
	m := &re2Matcher{pats: make([]re2Pat, 0, len(pats))}
	var bad []BadPattern
	for _, p := range pats {
		re, err := core.CompilePattern(p.Expr, p.Flags)
		if err != nil {
			bad = append(bad, BadPattern{ID: p.ID, Err: err})
			continue
		}
		m.pats = append(m.pats, re2Pat{id: p.ID, re: re})
	}
	return m, bad
}

// Fan-out thresholds, both taken from measured flip points rather than picked
// (BenchmarkRE2ScanSmall, 12-core darwin, the deployment's own rule patterns):
//
//	2 patterns ×   512 B  →  19.5 µs sequential vs 21.4 µs parallel  (fan-out LOSES)
//	4 patterns ×    64 B  →   7.4 µs           vs  7.8 µs            (fan-out LOSES)
//	4 patterns ×   512 B  →  44.9 µs           vs 31.2 µs            (1.4×)
//	24 patterns ×  512 B  → 286.6 µs           vs 112.1 µs           (2.6×)
//	423 patterns × 400 KB →   4.31 s           vs  0.73 s            (5.9×)
//
// So the fan-out is gated above the largest measured losing point: at least four
// patterns AND at least 2048 byte×pattern units of work.
const (
	minParallelPatterns = 4
	minParallelWork     = 2048
)

// Scan finds every (pattern, segment) match. Patterns are independent, so the
// work is fanned out across cores once it is large enough to pay for the
// goroutines — the same reason a compliance proxy must not spend seconds of
// single-core regexp time in the caller's request path.
//
// Why fan out rather than prefilter with one combined pattern: a union
// alternation was measured and REFUTED. Go's regexp walks a union in one pass but
// with a state set proportional to the whole pattern set, so at 100 patterns the
// union was 13% SLOWER than the per-pattern loop and at 423 patterns 44% slower —
// and when anything matched it was 2–2.4× slower, having paid for the union pass
// and then the per-pattern demux anyway. Single-pass multi-pattern matching is
// what the Vectorscan build is for; on this path the honest win is concurrency.
//
// The parallel and sequential paths share scanPattern and merge in pattern order,
// so the returned hits are identical — same values, same order — either way.
func (m *re2Matcher) Scan(segments []string, firstOnly bool) []Hit {
	if !m.shouldParallelize(segments) {
		return m.scanSequential(segments, firstOnly)
	}
	return m.scanParallel(segments, firstOnly)
}

// shouldParallelize applies the measured thresholds above. Cheap: one pass over
// the segment lengths, no allocation.
func (m *re2Matcher) shouldParallelize(segments []string) bool {
	if len(m.pats) < minParallelPatterns || runtime.GOMAXPROCS(0) < 2 {
		return false
	}
	total := 0
	for _, s := range segments {
		total += len(s)
	}
	return total*len(m.pats) >= minParallelWork
}

func (m *re2Matcher) scanSequential(segments []string, firstOnly bool) []Hit {
	var hits []Hit
	for _, p := range m.pats {
		hits = append(hits, scanPattern(p, segments, firstOnly)...)
	}
	return hits
}

// scanParallel runs one pattern per work item over a bounded pool and writes each
// pattern's hits into its own slot, so the merge below is deterministic and needs
// no lock. Bounded by GOMAXPROCS: a request that fans out must not be able to
// spawn one goroutine per configured rule.
func (m *re2Matcher) scanParallel(segments []string, firstOnly bool) []Hit {
	workers := min(runtime.GOMAXPROCS(0), len(m.pats))
	per := make([][]Hit, len(m.pats))
	idx := make(chan int, len(m.pats))
	for i := range m.pats {
		idx <- i
	}
	close(idx)

	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for i := range idx {
				per[i] = scanPattern(m.pats[i], segments, firstOnly)
			}
		}()
	}
	wg.Wait()

	// Merged in pattern order, which is the order scanSequential produces.
	var hits []Hit
	for _, h := range per {
		hits = append(hits, h...)
	}
	return hits
}

// scanPattern is the single copy of the matching logic. Both paths call it, so
// the parallel path cannot drift from the sequential one.
func scanPattern(p re2Pat, segments []string, firstOnly bool) []Hit {
	var hits []Hit
	for si, seg := range segments {
		if firstOnly {
			if loc := p.re.FindStringIndex(seg); loc != nil {
				hits = append(hits, Hit{ID: p.id, Seg: si, Start: loc[0], End: loc[1]})
			}
			continue
		}
		for _, loc := range p.re.FindAllStringIndex(seg, -1) {
			hits = append(hits, Hit{ID: p.id, Seg: si, Start: loc[0], End: loc[1]})
		}
	}
	return hits
}
