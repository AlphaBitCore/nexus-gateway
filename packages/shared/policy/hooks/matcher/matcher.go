// Package matcher is the engine seam for rule-pack content scanning. One
// interface, two implementations behind it:
//
//   - RE2 (this file): pure Go via the standard `regexp` engine. It is the
//     differential-test oracle and the opt-in production residual for the
//     handful of patterns Vectorscan cannot serve (precise sub-group redaction).
//   - Vectorscan (added later, cgo, build-tagged): one compiled database per
//     direction, scanned in a single pass.
//
// Both implementations must produce identical (pattern, segment, span) results
// for the patterns they share — pinned by the rule-pack engine's differential
// test. The decision layer (winner / tags / severity→action) lives in the
// engine and is engine-agnostic; the Matcher only answers "which patterns
// matched where".
//
// See docs/superpowers/specs/2026-06-22-rulepack-engine-perf-design.md §3.
package matcher

// Pattern is one rule pattern handed to a Matcher at compile time. ID is a
// caller-assigned stable index (typically the rule's position in the engine's
// rule slice) used to demux hits back to rules; the Matcher treats it as opaque.
type Pattern struct {
	ID    int
	Expr  string
	Flags string
}

// Hit is one pattern firing within one scanned segment, with the matched byte
// span [Start,End) within that segment. The span is the whole match (sub-group
// spans are an RE2-only capability handled in the redact path, not here).
type Hit struct {
	ID    int
	Seg   int
	Start int
	End   int
}

// BadPattern records a pattern that failed to compile. The caller decides the
// fail-posture (the rule-pack engine skips+logs it, matching today's behavior);
// compilation never aborts the whole set.
type BadPattern struct {
	ID  int
	Err error
}

// Matcher scans text segments against a compiled, read-only pattern set. A
// compiled Matcher is immutable and safe for concurrent Scan calls.
type Matcher interface {
	// Scan reports every (pattern, segment) match with its whole-match span.
	// Order is unspecified — callers resolve rule precedence by Pattern.ID.
	// When firstOnly is true the Matcher may stop at the first hit per
	// (pattern, segment) — enough for a block/detect decision; redact callers
	// pass false to collect every span to mask.
	Scan(segments []string, firstOnly bool) []Hit
}

// CompleteScanner is an optional capability of a Matcher whose scan can be
// truncated mid-stream. The cgo (Vectorscan) matcher aborts its match callback
// on an allocation failure, leaving a PARTIAL hit set — fine for a presence-only
// detect/block decision, but UNSAFE for redaction, where a dropped hit means a
// rule whose matched content would silently go unmasked. ScanComplete returns
// the hits plus whether the scan ran to completion; the redaction path consults
// it and re-localises every rule when complete is false. Matchers that never
// truncate (the pure-Go RE2 matcher) do not implement this — callers treat a
// non-implementer as always complete.
type CompleteScanner interface {
	ScanComplete(segments []string, firstOnly bool) (hits []Hit, complete bool)
}
