package matcher

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The parallel scan exists to cut latency, and it is only allowed to do that if
// it cannot change a single verdict: a compliance matcher that finds a different
// set of hits depending on how many cores were free would be worse than a slow
// one. So the guard is differential — the two paths are called directly, past the
// size threshold, and their output must be identical VALUE FOR VALUE INCLUDING
// ORDER (the redact path applies spans in the order it receives them).
func TestRE2Scan_ParallelMatchesSequential(t *testing.T) {
	pats := []Pattern{
		{ID: 10, Expr: `(?i)\bATTORNEY[ -]CLIENT\b`},
		{ID: 11, Expr: `\bsecret-\d+\b`},
		{ID: 12, Expr: `(?i)password`},
		{ID: 13, Expr: `\bAKIA[0-9A-Z]{16}\b`},
		{ID: 14, Expr: `(?i)\bssn\b`},
		{ID: 15, Expr: `\d{3}-\d{2}-\d{4}`},
	}
	m, bad := CompileRE2(pats)
	if len(bad) != 0 {
		t.Fatalf("bad patterns: %+v", bad)
	}
	re2 := m.(*re2Matcher)

	segmentSets := [][]string{
		{},
		{""},
		{"nothing sensitive here at all"},
		{"secret-42"},
		{"my SSN is 123-45-6789 and my password is hunter2"},
		{"attorney-client material", "AKIAIOSFODNN7EXAMPLE", "clean"},
		// Repeated matches in one segment: exercises the FindAll branch.
		{"secret-1 secret-2 secret-3", "password password"},
		// A long benign segment with one match at the very end, where a
		// per-pattern early exit and a fanned-out scan could most easily differ.
		{strings.Repeat("benign filler text. ", 500) + "secret-9999"},
	}

	for i, segs := range segmentSets {
		for _, firstOnly := range []bool{true, false} {
			name := fmt.Sprintf("set%d/firstOnly=%v", i, firstOnly)
			t.Run(name, func(t *testing.T) {
				want := re2.scanSequential(segs, firstOnly)
				got := re2.scanParallel(segs, firstOnly)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("parallel hits differ from sequential\n got: %+v\nwant: %+v", got, want)
				}
			})
		}
	}
}

// Scan must route to the parallel path only above the measured flip point, and
// must produce the same answer on both sides of it — otherwise the threshold
// itself becomes a behaviour switch.
func TestRE2Scan_ThresholdRoutesWithoutChangingResults(t *testing.T) {
	manyPats := make([]Pattern, 0, 8)
	for i := range 8 {
		manyPats = append(manyPats, Pattern{ID: i, Expr: fmt.Sprintf(`\bmarker%d\b`, i)})
	}
	m, _ := CompileRE2(manyPats)
	re2 := m.(*re2Matcher)

	tiny := []string{"marker3"}                                    // 7 B × 8 pats = 56  → below minParallelWork
	big := []string{strings.Repeat("x", 400) + " marker3 marker5"} // > 2048 byte×pats → above

	if re2.shouldParallelize(tiny) {
		t.Errorf("tiny input (%d byte×patterns) must not fan out", len(tiny[0])*len(re2.pats))
	}
	if !re2.shouldParallelize(big) {
		t.Errorf("large input (%d byte×patterns) must fan out", len(big[0])*len(re2.pats))
	}

	// Two patterns can never fan out regardless of size — below minParallelPatterns.
	few, _ := CompileRE2(manyPats[:2])
	if few.(*re2Matcher).shouldParallelize(big) {
		t.Error("a two-pattern set must not fan out; the measured flip point is 4")
	}

	// Same answers on both sides of the threshold.
	if got, want := re2.Scan(tiny, true), re2.scanSequential(tiny, true); !reflect.DeepEqual(got, want) {
		t.Errorf("below threshold: Scan = %+v, want %+v", got, want)
	}
	if got, want := re2.Scan(big, true), re2.scanSequential(big, true); !reflect.DeepEqual(got, want) {
		t.Errorf("above threshold: Scan = %+v, want %+v", got, want)
	}
}

// Every pattern's hits must survive the fan-out. The failure this catches is a
// merge that drops or overwrites a slot — which would silently mean "this rule
// never fires when the box is busy".
func TestRE2Scan_ParallelKeepsEveryPatternsHits(t *testing.T) {
	const n = 32
	pats := make([]Pattern, 0, n)
	var sb strings.Builder
	sb.WriteString(strings.Repeat("filler ", 200))
	for i := range n {
		pats = append(pats, Pattern{ID: 100 + i, Expr: fmt.Sprintf(`\bhit%d\b`, i)})
		fmt.Fprintf(&sb, "hit%d ", i)
	}
	m, _ := CompileRE2(pats)
	re2 := m.(*re2Matcher)
	segs := []string{sb.String()}

	if !re2.shouldParallelize(segs) {
		t.Fatal("fixture must be above the fan-out threshold or it proves nothing")
	}
	hits := re2.Scan(segs, true)
	if len(hits) != n {
		t.Fatalf("got %d hits, want one per pattern (%d)", len(hits), n)
	}
	seen := make(map[int]bool, n)
	for _, h := range hits {
		seen[h.ID] = true
	}
	for i := range n {
		if !seen[100+i] {
			t.Errorf("pattern %d produced no hit after the fan-out", 100+i)
		}
	}
}
