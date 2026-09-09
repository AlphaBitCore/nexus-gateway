package modela

import (
	"log/slog"
	"sync"
)

// DefaultTailWindowBytes is the package default trailing-window budget (the value
// withDefaults applies when Config.TailWindowBytes is unset). Exported so a substrate can
// compare a rule set's derived MaxPatternBytes against the window the engine will clamp the
// flush-before-deliver lookahead below (see withDefaults) and warn the operator when a
// contiguous enforceable pattern meets or exceeds it.
const DefaultTailWindowBytes = defaultTailWindowBytes

// StreamingCoverageGap reports whether a rule set's derived contiguous-pattern
// bound leaves Model-A unable to give its firm in-window guarantee at the given
// tail window. It is the predicate WarnStreamingCoverageGap logs on, exported so
// a substrate can size its window by the same rule it is judged by.
//
// The condition is the package doc's, not a looser proxy for it:
//
//	sound  ⟺  tailWindow > maxBounded + PrescanBatchBytes
//
// This used to test `maxBounded >= tailWindow` — the point at which withDefaults
// CLAMPS the lookahead. That is a strictly later point, and the difference is a
// band one PrescanBatchBytes wide sitting just under the window where the engine
// silently stops guaranteeing what it says it guarantees: the prescan runs only
// after PrescanBatchBytes of NEW content, and by then the window may already have
// evicted — that is, DELIVERED — the start of the value that just completed. The
// bounded-fragment leak the engine discloses for values LARGER than the window
// happens there to a value SMALLER than it.
//
// It was not a hypothetical band. The shipped rule pack derives 7362 bytes
// against an 8192-byte window, which is inside it, so the one configuration the
// product actually ships was the one the warning could not see.
//
// maxUnitSize is deliberately not a parameter: the substrate knows it, the
// engine does not, and a caller that sizes its window from this predicate should
// leave headroom above it rather than pass a number it can only guess at.
func StreamingCoverageGap(maxBounded, tailWindow int) bool {
	return tailWindow <= maxBounded+DefaultPrescanBatchBytes
}

// TailWindowFor sizes the tail window from a rule set's derived contiguous-pattern
// bound, so the window a substrate runs with satisfies StreamingCoverageGap by
// construction instead of by a constant that happened to be large enough when it
// was chosen.
//
// It never returns less than DefaultTailWindowBytes: shrinking the window for a
// small rule set would trade coverage for latency, and that trade is not this
// function's to make.
//
// The headroom above the bare condition is one PrescanBatchBytes. The condition's
// third term, maxUnitSize, is a per-stream property no config can bound — the SSE
// scanner admits frames far larger than any window worth holding — so it is the
// engine's disclosed best-effort surface rather than something to size against.
// What the headroom buys is that a unit up to a full prescan batch, which is
// already orders of magnitude above the one-token frame a chat stream produces,
// still lands inside the firm guarantee.
// The cost is first-byte latency in a narrow band, and it is a PRICE rather than
// a regression. Nothing is delivered in real time until accumulated content
// exceeds the window (the eviction trigger in the engine), so raising the window
// from the old 8192 constant to the derived 9410 the shipped rule pack implies
// moves that threshold: a response whose total redactable content lands in
// (8192, 9410] now waits for EOF instead of starting to flow at 8192. Measured
// at +1218 bytes held per concurrent stream (+14.9%).
//
// Delivering earlier is what was unsound. 8192 is below maxPattern +
// prescanBatch = 8386 for that same rule pack, so the content released at the
// old threshold could still carry an incomplete straddling value — which is the
// gap StreamingCoverageGap exists to report. A reader who arrives here because
// "streaming got slower in that size band" has found the trade, not a bug.
func TailWindowFor(maxPattern int) int {
	if want := maxPattern + 2*DefaultPrescanBatchBytes; want > DefaultTailWindowBytes {
		return want
	}
	return DefaultTailWindowBytes
}

// coverageWarnSeen dedupes the streaming-coverage Warn so a busy stream does not log on
// every setup. It is scoped to ONE rule-set generation and replaced wholesale when the
// generation advances.
//
// It used to be a package-level sync.Map keyed by the derived bound alone, with no eviction
// and the stated reasoning that the key space — bounds derived from admin-authored rule
// sets — is finite. Finite was true; what it missed is that the same key BECOMES TRUE
// AGAIN. An admin whose rule set derives 7362 bytes gets warned once, narrows the rule so
// the bound drops and the gap closes, then re-adds the long pattern — and the gap is back
// with the process permanently silent about it, because that bound had been seen. The
// warning was speaking once per bound per PROCESS, while what it has to say is a property
// of the rule set in force.
//
// The generation is the single truth point for "the rule set changed": it advances on every
// config push, and the pipeline that derived the bound carries it (Pipeline.RuleSetGeneration).
// Keying on it also bounds the memory the old map did not: one generation's distinct bounds
// live at a time, and the previous generation's set is dropped rather than accumulated.
var coverageWarnSeen struct {
	mu   sync.Mutex
	gen  uint64
	seen map[int]struct{}
}

// coverageWarnFirstSight reports whether this (generation, bound) pair has not been warned
// about yet, and records it. A generation change resets the set — that is the whole point,
// so it is done here rather than left to a caller to remember.
func coverageWarnFirstSight(gen uint64, maxBounded int) bool {
	coverageWarnSeen.mu.Lock()
	defer coverageWarnSeen.mu.Unlock()
	if coverageWarnSeen.seen == nil || coverageWarnSeen.gen != gen {
		coverageWarnSeen.gen = gen
		coverageWarnSeen.seen = map[int]struct{}{}
	}
	if _, ok := coverageWarnSeen.seen[maxBounded]; ok {
		return false
	}
	coverageWarnSeen.seen[maxBounded] = struct{}{}
	return true
}

// WarnStreamingCoverageGap emits a once-per-rule-set operator warning when
// StreamingCoverageGap holds for the rule set's longest CONTIGUOUS enforceable pattern
// (maxBounded — already derived by the substrate via pipeline.MaxPatternBound and threaded
// in; this never recomputes it) at the window in force. Model-A real-time streaming is then
// only best-effort for such a pattern: a value that long straddling unit boundaries may leak
// a bounded fragment before its completion is observed (the engine's disclosed surface).
//
// The remediation is deliberately NOT "raise the tail window by hand" — the window is not an
// admin knob. A substrate sizes it from the rule set via TailWindowFor; what remains after
// that is a pattern so long the window would have to grow past anything worth holding, and
// the answer for it is BUFFERED streaming mode (full coverage) or a narrower rule.
//
// Now genuinely silent in normal operation, which it was not before: the predicate used to
// be `maxBounded >= tailWindow`, and the shipped rule pack derives 7362 bytes against an
// 8192-byte window — a real gap, inside the band that predicate could not see. It is
// observability-only — off the per-byte path (called once at stream setup) and changes no
// enforcement.
//
// ruleSetGen scopes the dedupe. It is the generation the bound was derived under
// (Pipeline.RuleSetGeneration), and passing it is what makes the warning speak once per RULE
// SET rather than once per process: an operator who narrows a rule and later widens it again
// hears about the reopened gap, which the process-lifetime dedupe swallowed. Callers with no
// generation to hand may pass 0 — they then get the old once-per-process behaviour for that
// bound, which is the weaker guarantee but never a louder log.
//
// The dedupe holds a mutex across the check-and-record so two concurrent first-sight streams
// warn once.
func WarnStreamingCoverageGap(logger *slog.Logger, ruleSetGen uint64, maxBounded, tailWindow int) {
	if logger == nil || !StreamingCoverageGap(maxBounded, tailWindow) {
		return
	}
	if !coverageWarnFirstSight(ruleSetGen, maxBounded) {
		return
	}
	logger.Warn("streaming compliance: a loaded rule set's longest contiguous enforceable pattern meets or exceeds the streaming tail window; Model-A real-time streaming is best-effort for it — route the policy through buffered streaming mode for full coverage, or narrow the rule",
		"maxPatternBytes", maxBounded,
		"tailWindowBytes", tailWindow,
		"ruleSetGeneration", ruleSetGen,
	)
}
