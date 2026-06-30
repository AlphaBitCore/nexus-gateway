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

// coverageWarnSeen dedupes the streaming-coverage Warn by derived bound so a busy stream
// does not log on every setup. The key space is the set of distinct derived pattern bounds
// across loaded rule sets — admin-authored and finite — so no eviction is needed.
var coverageWarnSeen sync.Map // maxBounded int -> struct{}

// WarnStreamingCoverageGap emits a one-time (per distinct maxBounded) operator warning when
// a rule set's longest CONTIGUOUS enforceable pattern (maxBounded — already derived by the
// substrate via pipeline.MaxPatternBound and threaded in; this never recomputes it) meets or
// exceeds the streaming tail window. At/above that size the engine clamps the
// flush-before-deliver lookahead below the window, so Model-A real-time streaming is only
// best-effort for such a pattern: a value that long straddling unit boundaries may leak a
// bounded fragment before its completion is observed (the engine's disclosed surface).
//
// The remediation is deliberately NOT "raise the tail window" — the window is not an admin
// knob. Route the affected policy through BUFFERED streaming mode (full coverage) or narrow
// the >window-bounded rule.
//
// Silent in normal operation (realistic PII / token / JWT patterns sit well under the
// window); fires only on a genuinely unusual rule set. It is observability-only — off the
// per-byte path (called once at stream setup) and changes no enforcement. The dedupe uses
// LoadOrStore so two concurrent first-sight streams warn exactly once.
func WarnStreamingCoverageGap(logger *slog.Logger, maxBounded, tailWindow int) {
	if logger == nil || maxBounded < tailWindow {
		return
	}
	if _, loaded := coverageWarnSeen.LoadOrStore(maxBounded, struct{}{}); loaded {
		return
	}
	logger.Warn("streaming compliance: a loaded rule set's longest contiguous enforceable pattern meets or exceeds the streaming tail window; Model-A real-time streaming is best-effort for it — route the policy through buffered streaming mode for full coverage, or narrow the rule",
		"maxPatternBytes", maxBounded,
		"tailWindowBytes", tailWindow,
	)
}
