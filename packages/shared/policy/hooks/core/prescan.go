package core

// PrescanPattern is one anchor-stripped prefilter pattern a content hook
// contributes to the pipeline-wide union prefilter. Expr/Flags mirror the
// matcher.Pattern fields but are plain strings so core carries no dependency on
// the matcher package (which itself imports core — the reverse edge would be a
// cycle).
type PrescanPattern struct {
	Expr  string
	Flags string
}

// PrescanPatternSource is the optional capability that lets the pipeline fold
// every content hook's MayMatchRaw prefilter into ONE shared raw-body scan
// instead of one cgo scan per hook (the profiled hot spot under hooks-on load).
// A hook exports the EXACT anchor-stripped pattern set its own MayMatchRaw uses;
// the pipeline unions them, scans the raw body once, and the boolean result is
// identical to OR-ing every hook's MayMatchRaw. A hook returns nil to opt out
// (e.g. its prefilter could not be built, so it must conservatively force true);
// the pipeline then keeps the per-hook scan for that hook set — soundness over
// the optimisation.
type PrescanPatternSource interface {
	RawContentPrescanner
	PrescanPatterns() []PrescanPattern
}
