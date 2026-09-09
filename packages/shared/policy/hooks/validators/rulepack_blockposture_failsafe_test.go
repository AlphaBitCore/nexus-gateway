package validators

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// unlookedMatcher stands in for the engine's matcher after a rule-pack swap has
// closed it while an in-flight request still holds the engine — the state
// vectorscanMatcher reports as (nil, false): no hits, and "this call did not
// look". The redact path already consults that flag; these tests are about the
// block/detect path, which reads only the hit set.
type unlookedMatcher struct{}

func (unlookedMatcher) Scan(_ []string, _ bool) []matcher.Hit { return nil }

func (unlookedMatcher) ScanComplete(_ []string, _ bool) ([]matcher.Hit, bool) { return nil, false }

// blockingEngine builds a rule-pack engine bound to a hard-severity blocking
// rule — the shape an operator gets when they install a compliance pack.
func blockingEngine(t *testing.T, m matcher.Matcher) *RulePackEngine {
	t.Helper()
	cfg := &core.HookConfig{
		ID:               "hook-rulepack-block",
		ImplementationID: "rulepack-engine",
		Name:             "compliance pack",
		Config: map[string]any{
			"_rulePackInstalls": []rulePackInstall{{
				InstallID:   "install-1",
				PackName:    "safety-default",
				PackVersion: "1.0.0",
				Enabled:     true,
				Rules: []rulePackRule{{
					RuleID:   "no-ssn",
					Category: "pii",
					Severity: "hard",
					Pattern:  `\b\d{3}-\d{2}-\d{4}\b`,
				}},
			}},
			"onMatch": map[string]any{"action": "block"},
		},
	}
	h, err := NewRulePackEngine(cfg)
	if err != nil {
		t.Fatalf("building the engine: %v", err)
	}
	e := h.(*RulePackEngine)
	if m != nil {
		if c, ok := e.matcher.(interface{ Close() error }); ok {
			_ = c.Close()
		}
		e.matcher = m
	}
	return e
}

func execute(t *testing.T, e *RulePackEngine, text string) *core.HookResult {
	t.Helper()
	res, err := e.Execute(context.Background(), &core.HookInput{
		Stage: "request",
		Normalized: &normalize.NormalizedPayload{
			Kind:             normalize.KindAIChat,
			NormalizeVersion: normalize.SchemaVersion,
			Messages: []normalize.Message{{
				Role:    normalize.RoleUser,
				Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: text}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

// TestRulePackEngine_BlockPostureFailsSafeOnAnUnlookedScan pins the invariant
// that a scan which never ran may not be read as "no rule fired".
//
// The redact branch consults the completeness flag; the enforcement loop below
// it reads only the hit set, so an unrepaired empty set makes the engine return
// Approve for exactly the content the pack is bound to block — and the audit
// row then records an approve that asserts a scan happened, which is worse than
// a visible failure.
func TestRulePackEngine_BlockPostureFailsSafeOnAnUnlookedScan(t *testing.T) {
	e := blockingEngine(t, unlookedMatcher{})

	res := execute(t, e, "my social is 123-45-6789, please keep it safe")

	if res.Decision == core.Approve {
		t.Fatal("the engine approved content its hard-severity rule matches, because the " +
			"matcher reported an empty hit set for a scan that never ran")
	}
	if res.BlockingRule == nil || res.BlockingRule.RuleID != "no-ssn" {
		t.Fatalf("expected the blocking rule to be attributed to no-ssn, got %+v", res.BlockingRule)
	}
}

// TestRulePackEngine_UnlookedScanStillApprovesCleanContent is the other half.
// The fail-safe re-runs the rules through RE2; it must not degrade into
// "block everything when the accelerator is unavailable", which would turn a
// swap window into an outage.
func TestRulePackEngine_UnlookedScanStillApprovesCleanContent(t *testing.T) {
	e := blockingEngine(t, unlookedMatcher{})

	res := execute(t, e, "no sensitive numbers in this sentence at all")

	if res.Decision != core.Approve {
		t.Fatalf("clean content must still be approved through the fail-safe path, got %v (%s)",
			res.Decision, res.Reason)
	}
}

// countingMatcher wraps a real matcher and records how it was consulted, so a
// test can assert WHICH path a request took rather than only what it decided.
type countingMatcher struct {
	inner    matcher.Matcher
	complete bool
	scans    int
}

func (c *countingMatcher) Scan(segments []string, firstOnly bool) []matcher.Hit {
	c.scans++
	return c.inner.Scan(segments, firstOnly)
}

func (c *countingMatcher) ScanComplete(segments []string, firstOnly bool) ([]matcher.Hit, bool) {
	c.scans++
	hits := c.inner.Scan(segments, firstOnly)
	return hits, c.complete
}

// TestRulePackEngine_HealthyMatcherIsNotRescanned keeps the repair off the hot
// path: a complete scan must be used as-is, never re-confirmed through RE2.
// Without this the fix would quietly pay the per-rule regex cost the shared
// matcher exists to avoid, on every request.
//
// This asserts the PATH, not just the decision. An earlier version of this test
// checked only that a healthy matcher blocks matching content and approves
// clean content — both of which hold identically if matchedByRE2 ran on every
// request, so it could not fail for the reason its own comment named.
func TestRulePackEngine_HealthyMatcherIsNotRescanned(t *testing.T) {
	real := blockingEngine(t, nil).matcher // the real compiled matcher
	cm := &countingMatcher{inner: real, complete: true}
	e := blockingEngine(t, cm)

	res := execute(t, e, "my social is 123-45-6789")
	if res.Decision == core.Approve {
		t.Fatal("a healthy matcher must still block matching content")
	}
	if cm.scans != 1 {
		t.Fatalf("expected exactly one accelerated scan, got %d", cm.scans)
	}
	if res.BlockingRule == nil {
		t.Fatal("the block must carry its rule attribution")
	}

	// The control that proves the assertion above can fail: the SAME wrapper
	// reporting an incomplete scan must take the repair path, which is visible
	// as the engine still blocking on a hit set the wrapper reported as empty.
	cmIncomplete := &countingMatcher{inner: emptyMatcher{}, complete: false}
	eIncomplete := blockingEngine(t, cmIncomplete)
	if res := execute(t, eIncomplete, "my social is 123-45-6789"); res.Decision == core.Approve {
		t.Fatal("the repair path did not run, so this test cannot tell the two paths apart")
	}

	if res := execute(t, e, "nothing sensitive here"); res.Decision != core.Approve {
		t.Fatalf("a healthy matcher must still approve clean content, got %v", res.Decision)
	}
}

// emptyMatcher reports nothing, so a decision made on its output can only have
// come from the RE2 repair.
type emptyMatcher struct{}

func (emptyMatcher) Scan(_ []string, _ bool) []matcher.Hit { return nil }

// TestRulePackEngine_MetricDescribesTheDecision pins the content-scan counter to
// the set the decision is made from.
//
// ObserveContentScan flags a scan as BENIGN when the match set is empty. Called
// before the repair, it flagged exactly the requests the engine then blocked —
// so through a rule-pack swap the benign counter climbed while RULEPACK_MATCH
// blocks fired, and the metric contradicted the decision it describes.
func TestRulePackEngine_MetricDescribesTheDecision(t *testing.T) {
	e := blockingEngine(t, unlookedMatcher{})
	benign := core.ContentScanBenignTotal.WithLabelValues("rulepack-engine")
	before := testutil.ToFloat64(benign)

	res := execute(t, e, "my social is 123-45-6789")
	if res.Decision == core.Approve {
		t.Fatal("fixture no longer blocks, so this test cannot see the contradiction it guards")
	}

	if got := testutil.ToFloat64(benign) - before; got != 0 {
		t.Fatalf("a request the engine BLOCKED was counted as a benign scan (+%v). "+
			"An operator watching this counter through a swap window sees no scan "+
			"activity while blocks are firing.", got)
	}
}

// The control: a genuinely clean request must still count as benign, or the
// assertion above is satisfiable by never counting anything.
func TestRulePackEngine_CleanContentStillCountsAsBenign(t *testing.T) {
	e := blockingEngine(t, unlookedMatcher{})
	benign := core.ContentScanBenignTotal.WithLabelValues("rulepack-engine")
	before := testutil.ToFloat64(benign)

	if res := execute(t, e, "no sensitive numbers at all"); res.Decision != core.Approve {
		t.Fatalf("clean content must be approved, got %v", res.Decision)
	}
	if got := testutil.ToFloat64(benign) - before; got != 1 {
		t.Fatalf("a clean scan must count as benign exactly once, got +%v", got)
	}
}
