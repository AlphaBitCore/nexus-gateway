package matcher

// prefilter_lint.go — author-time diagnosis of whether a rule pattern is friendly
// to the raw-body prefilter (the perf path that skips structured extraction on
// benign traffic, see the ai-gateway hook stage). A hostile pattern does not break
// correctness — the hook still runs — but it silently degrades the prefilter:
// either the whole hook's prescan collapses (StripAnchors / compile failure ⇒ the
// hook can never let the proxy skip extraction), or the anchor-stripped superset
// matches everything (the prefilter fires on every request ⇒ no speedup + high
// false-positive pressure). Surfacing this to a rule author at edit time is the
// point: keep the fast path fast. The diagnosis mirrors newContentPrescan's own
// degrade conditions so author-time and runtime agree.

import "regexp"

// PrefilterCode classifies a pattern's prefilter friendliness.
type PrefilterCode string

const (
	// PrefilterFriendly: the pattern anchor-strips and compiles to a selective
	// superset — the prefilter can skip extraction on bodies it does not match.
	PrefilterFriendly PrefilterCode = ""
	// PrefilterUnparseable: not a valid regular expression — it cannot be
	// anchor-stripped for the prefilter, so a hook carrying it forces full
	// per-request extraction (and fails to compile at the hook too).
	PrefilterUnparseable PrefilterCode = "unparseable"
	// PrefilterUncompilableSuperset: the anchor-stripped form does not compile, so
	// the hook's prefilter degrades and forces full extraction. Defensive — the
	// stripped form is a re-rendered valid parse tree, so this is not normally
	// reachable, but regexp.Compile's own size limits are honored rather than
	// assumed away.
	PrefilterUncompilableSuperset PrefilterCode = "uncompilable_superset"
	// PrefilterAlwaysMatch: the anchor-stripped superset matches empty input, so
	// the prefilter fires on every request (no speedup) and the pattern is prone
	// to false positives.
	PrefilterAlwaysMatch PrefilterCode = "always_match"
)

// PrefilterDiagnosis is the result of DiagnosePrefilter. Friendly is the headline;
// Code/Message explain a non-friendly verdict for a UI warning or an audit report.
type PrefilterDiagnosis struct {
	Friendly bool
	Code     PrefilterCode
	Message  string
}

// DiagnosePrefilter reports whether expr is friendly to the raw-body prefilter.
// It never panics and treats any failure as a (non-fatal) hostile verdict — the
// caller surfaces it as a warning, never a hard rejection (a hostile pattern still
// works, just without the fast path). Pure Go (no build tag) so the control plane
// can call it at rule-edit time and an offline audit can call it over seed data.
func DiagnosePrefilter(expr string) PrefilterDiagnosis {
	// StripAnchors parses expr (RE2/PerlX) and rewrites it into the prefilter
	// superset; a parse failure is the single "unparseable" signal — the same
	// failure that collapses the hook's prescan (newContentPrescan) and that the
	// hook's own matcher would hit.
	stripped, err := StripAnchors(expr)
	if err != nil {
		return PrefilterDiagnosis{Code: PrefilterUnparseable,
			Message: "pattern is not a valid regular expression: " + err.Error()}
	}
	re, err := regexp.Compile(stripped)
	if err != nil {
		return PrefilterDiagnosis{Code: PrefilterUncompilableSuperset,
			Message: "the anchor-stripped form does not compile; the hook's prefilter " +
				"degrades and forces full extraction"}
	}
	if re.MatchString("") {
		return PrefilterDiagnosis{Code: PrefilterAlwaysMatch,
			Message: "the anchor-stripped form matches empty input, so the prefilter " +
				"fires on every request (no speedup) and the pattern is prone to false " +
				"positives — tighten it (drop leading/trailing wildcards or fully-optional quantifiers)"}
	}
	return PrefilterDiagnosis{Friendly: true}
}
