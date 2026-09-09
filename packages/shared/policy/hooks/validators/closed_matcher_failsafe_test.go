package validators

import (
	"context"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
)

// closedMatcher reports "scanned to completion, zero matches" for a scan that
// never ran — the exact shape a real accelerated matcher takes on after a
// rule-pack swap closes it while an in-flight request still holds it.
//
// It implements CompleteScanner so a caller CAN tell the two apart; the defect
// these tests pin is a caller that does not ask.
type closedMatcher struct{}

func (closedMatcher) Scan(_ []string, _ bool) []matcher.Hit { return nil }
func (closedMatcher) ScanComplete(_ []string, _ bool) ([]matcher.Hit, bool) {
	return nil, false
}

var _ matcher.CompleteScanner = closedMatcher{}

const piiEmailPattern = `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`

// TestPiiDetector_ClosedMatcherStillMasks is the leak.
//
// The detection gate is a fast path: one accelerated scan over every pattern,
// and "nothing fired" short-circuits to Approve without any RE2 work. But an
// empty hit set from a matcher that was CLOSED is not "nothing fired" — it is
// "we did not look", and the two were indistinguishable. A redact hook whose
// entire job is masking PII therefore approved the payload untouched.
//
// The assertion is on the OUTCOME, not on the hit set: the segment carries an
// address the pattern matches, so a correct run must not answer Approve.
func TestPiiDetector_ClosedMatcherStillMasks(t *testing.T) {
	h, err := NewPiiDetector(makePiiConfig([]map[string]any{
		{"id": "email", "regex": piiEmailPattern, "flags": "i"},
	}, "redact"))
	if err != nil {
		t.Fatalf("NewPiiDetector: %v", err)
	}
	pd, ok := h.(*PiiDetector)
	if !ok {
		t.Fatalf("want *PiiDetector, got %T", h)
	}

	const secret = "contact alice@example.com now"

	// Precondition: with a working matcher the hook masks.
	before, err := pd.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{secret}),
	})
	if err != nil {
		t.Fatalf("Execute (open): %v", err)
	}
	if before.Decision == Approve {
		t.Fatalf("precondition failed: an open matcher approved text containing an email")
	}

	// The matcher is closed underneath an in-flight request.
	pd.matcher = closedMatcher{}

	after, err := pd.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{secret}),
	})
	if err != nil {
		t.Fatalf("Execute (closed): %v", err)
	}
	if after.Decision == Approve {
		t.Fatalf("a closed matcher approved text containing PII — the payload leaves unmasked while the scan never ran (decision=%v)", after.Decision)
	}
}

// TestMayMatchRaw_ClosedPrescanIsConservative — the prefilter answers a
// different question, and a wrong answer is worse: returning false tells the
// proxy "no rule can match this body", so it skips extraction and bypasses the
// hook entirely. A nil prefilter is already conservative-true for exactly this
// reason; a closed one has to be too.
func TestMayMatchRaw_ClosedPrescanIsConservative(t *testing.T) {
	c := contentPrescan{prescan: closedMatcher{}}
	if !c.MayMatchRaw([]byte(`{"messages":[{"content":"alice@example.com"}]}`)) {
		t.Fatal("a closed prescan reported that no rule could match — the proxy would skip extraction and bypass the hook")
	}
	// An empty body still short-circuits: there is nothing to match, and that
	// is a fact about the body rather than about the matcher.
	if c.MayMatchRaw(nil) {
		t.Error("an empty body must not be reported as possibly matching")
	}
}

// TestMatchedSetOrConfirm_RebuildsWithRE2 pins the recovery the block-posture
// hooks depend on: they consult the matched set AS the decision, so a truncated
// scan cannot simply be passed through. RE2 is the oracle the accelerator may
// be faster than, never less complete than.
func TestMatchedSetOrConfirm_RebuildsWithRE2(t *testing.T) {
	pats := []matcher.Pattern{{ID: 0, Expr: piiEmailPattern, Flags: "i"}}
	segments := []string{"nothing here", "contact alice@example.com now"}

	got := matchedSetOrConfirm(closedMatcher{}, pats, segments)
	if _, ok := got[[2]int{0, 1}]; !ok {
		t.Fatalf("the truncated scan was not re-confirmed with RE2; matched set = %v", got)
	}
	if _, ok := got[[2]int{0, 0}]; ok {
		t.Error("the rebuild reported a match in a segment that contains none")
	}
}
