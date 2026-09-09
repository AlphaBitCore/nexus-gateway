// Package proxy — the Model-A tail window must satisfy the engine's own
// soundness condition for the rule set this product ships.
//
// Named failure modes:
//   - a sensitive value SHORTER than the tail window has its leading bytes
//     delivered raw before the rule that matches it fires
//   - the operator warning that exists to announce a coverage gap cannot see
//     the gap the shipped rule set actually produces
package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/modela"
)

// seedFixtures is the rule set every install starts with. The soundness of the
// streaming window is a property of THIS pack, not of a hypothetical one, so
// the bound is measured from the shipped bytes rather than assumed to be small.
const seedFixtures = "../../../../../tools/db-migrate/seed/fixtures/"

// derivedBoundFromSeed reproduces what production computes: the longest
// contiguous enforceable match across the enabled packs, anchor-stripped the
// way newContentPrescan strips before the pipeline measures it, floored at the
// engine's default the way buildResponsePrescan floors it.
func derivedBoundFromSeed(t *testing.T) (maxPattern int, longestRule string) {
	t.Helper()
	read := func(name string, out any) {
		raw, err := os.ReadFile(filepath.Clean(seedFixtures + name))
		if err != nil {
			t.Fatalf("read seed fixture %s: %v", name, err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("parse seed fixture %s: %v", name, err)
		}
	}

	var installs []struct {
		PackID  string `json:"packId"`
		Enabled bool   `json:"enabled"`
	}
	read("rule_pack_install.json", &installs)
	enabled := map[string]bool{}
	for _, i := range installs {
		if i.Enabled {
			enabled[i.PackID] = true
		}
	}
	if len(enabled) == 0 {
		t.Fatal("no rule pack is installed enabled in the seed — the bound would be measured " +
			"over an empty set and this test would pass without testing anything")
	}

	var rules []struct {
		RuleID  string `json:"ruleId"`
		PackID  string `json:"packId"`
		Pattern string `json:"pattern"`
	}
	read("rule.json", &rules)

	type sized struct {
		id string
		n  int
	}
	var bounded []sized
	scanned := 0
	for _, r := range rules {
		if !enabled[r.PackID] {
			continue
		}
		scanned++
		expr, err := matcher.StripAnchors(r.Pattern)
		if err != nil {
			// newContentPrescan abandons the whole prefilter in this case, which
			// makes every scan a full confirm. Worth failing loudly rather than
			// quietly measuring a smaller bound.
			t.Fatalf("rule %s cannot be anchor-stripped, which disables the union prefilter "+
				"entirely: %v", r.RuleID, err)
		}
		if n, ok := matcher.MaxMatchBytes(expr); ok {
			bounded = append(bounded, sized{r.RuleID, n})
		}
	}
	if scanned == 0 {
		t.Fatal("no seeded rule belongs to an enabled pack")
	}
	sort.Slice(bounded, func(i, j int) bool { return bounded[i].n > bounded[j].n })

	maxPattern = modela.DefaultMaxPatternBytes
	if len(bounded) > 0 && bounded[0].n > maxPattern {
		maxPattern, longestRule = bounded[0].n, bounded[0].id
	}
	return maxPattern, longestRule
}

// TestModelAWindowSatisfiesEngineSoundnessForShippedRules is the gate.
//
// engine.go states the condition in its own words:
//
//	Soundness for sub-MaxPatternBytes values requires
//	TailWindowBytes > MaxPatternBytes + PrescanBatchBytes + maxUnitSize
//
// Below that, the prescan can run only after PrescanBatchBytes of new content
// has arrived — by which time the window may already have evicted, and
// therefore DELIVERED, the start of the value that just completed. The result
// is the bounded-fragment leak the engine discloses for values LARGER than the
// window, happening to a value SMALLER than it.
func TestModelAWindowSatisfiesEngineSoundnessForShippedRules(t *testing.T) {
	maxPattern, rule := derivedBoundFromSeed(t)

	// The window the relay actually runs with. Recomputing the same expression
	// here is what made the previous version a tautology: TailWindowFor is
	// defined to satisfy the assertion, so the test held for every input and
	// held just as well when the relay had stopped calling it.
	window := gatewayTailWindow(maxPattern)
	need := maxPattern + modela.DefaultPrescanBatchBytes

	if window <= need {
		t.Errorf(
			"the shipped rule set derives MaxPatternBytes=%d (rule %s), and the Model-A tail "+
				"window in force is %d.\nEngine soundness needs window > maxPattern + "+
				"prescanBatch (+ maxUnitSize): %d > %d + %d = %d — short by %d bytes.\n"+
				"A value of that length streaming out has its leading bytes delivered raw "+
				"before the rule matching it can fire.",
			maxPattern, rule, window,
			window, maxPattern, modela.DefaultPrescanBatchBytes, need, need-window+1)
	}
	if window < modelATailWindowBytes {
		t.Errorf("the derived window %d fell below the floor %d — a small rule set must not "+
			"buy latency by shrinking coverage", window, modelATailWindowBytes)
	}
	t.Logf("shipped pack: maxPattern=%d (%s) -> window=%d (floor %d)",
		maxPattern, rule, window, modelATailWindowBytes)
}

// TestCoverageWarningSeesTheGapItAnnounces holds the operator signal to the
// same condition.
//
// The warning used to fire only when the derived bound met or exceeded the
// window — the case where the engine CLAMPS the lookahead. That leaves a band
// one PrescanBatchBytes wide, just under the window, where soundness fails and
// nothing is logged. The shipped rule set lands inside it, so the one
// configuration that actually produces the gap was the one the warning could
// not see.
func TestCoverageWarningSeesTheGapItAnnounces(t *testing.T) {
	const window = 8 * 1024
	cases := []struct {
		name        string
		maxPattern  int
		wantWarning bool
	}{
		{"comfortably inside the window", 512, false},
		{"at the soundness boundary, still silent", window - modela.DefaultPrescanBatchBytes - 1, false},
		{"inside the silent band the shipped pack lands in", 7362, true},
		{"one byte under the window", window - 1, true},
		{"at the window, where the lookahead is clamped", window, true},
		{"past the window", window + 4096, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modela.StreamingCoverageGap(tc.maxPattern, window); got != tc.wantWarning {
				t.Errorf("StreamingCoverageGap(maxPattern=%d, window=%d) = %v, want %v",
					tc.maxPattern, window, got, tc.wantWarning)
			}
		})
	}
}
