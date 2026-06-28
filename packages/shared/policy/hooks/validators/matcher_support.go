package validators

// Shared plumbing so every regex-configuring content hook (rulepack-engine and
// the keyword-filter / content-safety / pii-detector front-ends) executes its
// patterns through the same Matcher seam — Vectorscan under -tags vectorscan,
// pure-Go RE2 otherwise. Routing all of them through one seam is what makes the
// accelerator actually cover every configured regex, not just rule packs.

import (
	"unsafe"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
)

// buildMatcher compiles the pattern set through the build-tag-selected engine.
// Bad patterns (RE2-uncompilable) are returned, not fatal — each caller applies
// its own fail-posture (the front-ends reject at construction; rulepack-engine
// skips+logs).
func buildMatcher(pats []matcher.Pattern) (matcher.Matcher, []matcher.BadPattern) {
	return defaultCompiler()(pats)
}

// matchedSet scans the segments once and returns the set of (patternID,
// segmentIndex) pairs that fired. firstOnly=true: a detect/block decision only
// needs presence, not count. Callers iterate in their own precedence order and
// consult the set, so the engine choice never affects decision ordering.
func matchedSet(m matcher.Matcher, segments []string) map[[2]int]struct{} {
	set := make(map[[2]int]struct{})
	for _, h := range m.Scan(segments, true) {
		set[[2]int{h.ID, h.Seg}] = struct{}{}
	}
	return set
}

// contentPrescan is the raw-body prefilter shared by every content-scanning
// hook (rulepack-engine + the keyword-filter / content-safety / pii-detector
// front-ends). It compiles an anchor-stripped SUPERSET of the hook's pattern
// set; scanning that over the raw request bytes answers "could any rule match
// the content this body carries?" without the expensive structured extraction.
// Embed it into a hook struct to satisfy core.RawContentPrescanner.
type contentPrescan struct {
	// prescan is the anchor-stripped superset matcher, or nil when the set
	// could not be fully represented as a prefilter (a pattern that would not
	// parse / strip / compile). A nil prescan forces MayMatchRaw to return
	// true, so the hook never lets the proxy skip extraction on its behalf —
	// soundness over coverage.
	prescan matcher.Matcher

	// stripped is the exact anchor-stripped pattern set `prescan` was compiled
	// from, exported via PrescanPatterns so the pipeline can fold every content
	// hook's prefilter into ONE shared raw-body scan. Nil whenever prescan is nil
	// (the hook then opts out of the union and keeps its per-hook scan).
	stripped []core.PrescanPattern
}

// newContentPrescan builds the prefilter from the SAME pattern set the hook
// scans with, so the superset always covers exactly the hook's rules. If any
// pattern cannot be anchor-stripped or compiled into the prefilter, it returns a
// zero contentPrescan (nil matcher) — the conservative posture that never skips.
func newContentPrescan(pats []matcher.Pattern) contentPrescan {
	stripped := make([]matcher.Pattern, 0, len(pats))
	exported := make([]core.PrescanPattern, 0, len(pats))
	for _, p := range pats {
		expr, err := matcher.StripAnchors(p.Expr)
		if err != nil {
			return contentPrescan{}
		}
		stripped = append(stripped, matcher.Pattern{ID: p.ID, Expr: expr, Flags: p.Flags})
		exported = append(exported, core.PrescanPattern{Expr: expr, Flags: p.Flags})
	}
	m, bad := buildMatcher(stripped)
	if len(bad) > 0 {
		return contentPrescan{}
	}
	return contentPrescan{prescan: m, stripped: exported}
}

// PrescanPatterns exports the anchor-stripped pattern set this hook's MayMatchRaw
// prefilter uses, so the pipeline can union every content hook's prefilter into a
// single raw-body scan (core.PrescanPatternSource). Returns nil when no prefilter
// was built (prescan == nil) — the hook then keeps its own per-hook scan, since a
// nil prefilter means it must conservatively force a "may match".
func (c contentPrescan) PrescanPatterns() []core.PrescanPattern { return c.stripped }

// ScansContent reports that this hook's verdict depends on extracted request
// content, so the proxy may only skip extraction when MayMatchRaw is false.
func (c contentPrescan) ScansContent() bool { return true }

// MayMatchRaw reports whether any rule could match somewhere in the raw body.
// Conservative (true) when no prefilter was built. The body is scanned as a
// single segment via a zero-copy string view (read-only; the matcher never
// retains or mutates it).
func (c contentPrescan) MayMatchRaw(body []byte) bool {
	if c.prescan == nil {
		return true
	}
	if len(body) == 0 {
		return false
	}
	return len(c.prescan.Scan([]string{bytesView(body)}, true)) > 0
}

// bytesView returns a read-only string view over b without copying. The matcher
// only reads the segment during Scan and does not retain it, so the aliasing is
// safe and avoids a per-request 50KB allocation on the hot path.
func bytesView(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// Close releases the prefilter matcher's native resources (the Vectorscan
// database, if any). No-op for the RE2 matcher and for a nil prescan.
func (c contentPrescan) closePrescan() error {
	if cl, ok := c.prescan.(interface{ Close() error }); ok {
		return cl.Close()
	}
	return nil
}
