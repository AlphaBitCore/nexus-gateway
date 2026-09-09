package pipeline

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
)

// closedUnionMatcher is a matcher that was closed by a rule-pack swap while a
// pipeline built under the previous generation still held it. That is not a
// hypothetical: PolicyResolver.Swap closes the superseded generation's union
// matchers (closeUnionsIfGen) and a pipeline captured for an SSE response holds
// its pointer for the whole stream.
//
// It reports what the real vectorscanMatcher reports in that state — no hits,
// and complete=false, meaning "this call did not look". Scan is the lossy view
// of the same answer, which is exactly the trap: the hits alone are
// indistinguishable from a healthy scan that found nothing.
type closedUnionMatcher struct{ scans int }

func (m *closedUnionMatcher) Scan(segments []string, firstOnly bool) []matcher.Hit {
	hits, _ := m.ScanComplete(segments, firstOnly)
	return hits
}

func (m *closedUnionMatcher) ScanComplete(_ []string, _ bool) ([]matcher.Hit, bool) {
	m.scans++
	return nil, false
}

// TestMayMatchRawContent_ClosedUnionIsConservative pins the soundness invariant
// on the path that actually runs in production. MayMatchRawContent's false is
// load-bearing: the proxy reads it as "no bound hook can match this body" and
// skips the structured scan entirely, so every content hook abstains and the
// held frames are released raw.
//
// The union prefilter is the DEFAULT path (NEXUS_HOOK_UNION_PRESCAN and
// NEXUS_HOOK_PREFILTER both default on). The per-hook loop that the sibling
// test in validators covers runs only on the opt-out, on unstable hook IDs, or
// on a single-flight miss — so a fail-safe proven only there proves nothing
// about the deployed configuration.
func TestMayMatchRawContent_ClosedUnionIsConservative(t *testing.T) {
	m := &closedUnionMatcher{}
	p := &Pipeline{unionPrescan: m}

	if !p.MayMatchRawContent([]byte(`{"messages":[{"content":"anything at all"}]}`)) {
		t.Fatal("a closed union prefilter answered \"no bound hook can match this body\" " +
			"for a scan that never ran — the caller skips extraction and every content hook abstains")
	}
	if m.scans != 1 {
		t.Fatalf("expected the union to be consulted exactly once, got %d scans", m.scans)
	}

	// The empty body stays false: that answer comes from the length check, not
	// from a scan, so it is a real measurement and must not be widened by the
	// fail-safe. Widening it would send every empty body through the full
	// structured extraction for nothing.
	if p.MayMatchRawContent(nil) {
		t.Fatal("an empty body must still answer false — no scan is needed to know it carries no content")
	}
}

// TestMayMatchRawContent_HealthyUnionStillSkips is the other half of the pair:
// the fail-safe must not degrade into "always true", which would silently undo
// the union prefilter's entire reason for existing (one cgo scan instead of one
// per hook, and skipping extraction when nothing can match).
func TestMayMatchRawContent_HealthyUnionStillSkips(t *testing.T) {
	pats := []matcher.Pattern{{ID: 0, Expr: `secret-token-[0-9]+`}}
	m, bad := matcher.CompileDefault(pats)
	if len(bad) > 0 {
		t.Fatalf("fixture pattern did not compile: %v", bad)
	}
	defer func() {
		if c, ok := m.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	}()
	p := &Pipeline{unionPrescan: m}

	if p.MayMatchRawContent([]byte(`{"messages":[{"content":"nothing interesting here"}]}`)) {
		t.Fatal("a healthy union that found nothing must still answer false, or the prefilter buys nothing")
	}
	if !p.MayMatchRawContent([]byte(`{"messages":[{"content":"secret-token-42"}]}`)) {
		t.Fatal("a healthy union that found a match must answer true")
	}
}
