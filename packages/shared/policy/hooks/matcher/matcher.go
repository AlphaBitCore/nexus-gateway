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

// StreamScanner is an optional capability of a Matcher that can carry match
// state ACROSS successive writes, so content arriving in pieces is scanned once
// rather than re-scanned from the beginning on every new piece.
//
// The distinction is not an optimisation detail, it decides how much of a
// streamed response must be withheld from the client. A matcher without it can
// only answer "does this whole buffer match", so a caller watching a stream has
// to re-scan the accumulation — which is O(n²) over the response, forces the
// scan to be batched, and forces a tail to be held undelivered long enough to
// cover the batch. A matcher WITH it answers "has anything matched so far"
// after each piece, at the cost of that piece alone, so the caller need withhold
// only the piece it is currently deciding on.
//
// A Matcher that does not implement it is not deficient — RE2 has no streaming
// mode — and callers must keep the accumulate-and-rescan path for that case.
//
// Scoped to presence, deliberately. A stream reports only whether some pattern
// could have matched; it never reports WHERE. Match positions are what a
// redaction needs, and locating them is the expensive, heavily-constrained part
// of streaming regex engines. The division of labour that follows is the point:
// the cheap streaming prefilter says "look closer", and the existing block-mode
// Scan does the locating, on accumulated text, only when something said so.
type StreamScanner interface {
	// OpenScanStream begins a scan whose state persists across writes. The
	// caller must Close it; an unclosed stream leaks engine state.
	OpenScanStream() (ScanStream, error)
}

// ScanStream accumulates match state over content delivered in arrival order.
type ScanStream interface {
	// Write feeds the next piece of content and reports whether any pattern may
	// have matched ANY of the content written so far, this piece included. A
	// pattern spanning a boundary between two writes is found: that is the whole
	// reason the state is carried.
	//
	// Once it reports true it may keep reporting true; callers treat the first
	// true as the signal to run the authoritative scan.
	Write(chunk []byte) (mayMatch bool, err error)

	// Close releases the engine state. Safe to call once; idempotent.
	Close() error
}
