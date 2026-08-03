package matcher

// Engine describes the content-scanning engine THIS BINARY was built with, so a
// running service can be asked which one it got instead of the question being
// answerable only by inspecting the build.
//
// There is deliberately no "scanBounded" field. An earlier version had one,
// defined as "does the engine cap how much text a single scan examines", and set
// it true for Vectorscan — which is wrong: `hs_scan` reads the whole segment, and
// Vectorscan's only cap (`detectionRepeatCap`) bounds a PATTERN's repeat, not the
// text. It is false for RE2 as well, so the field was a constant that happened to
// be a false claim on the build production actually runs. A compliance owner
// reading it would have concluded that large bodies are not fully scanned for PII
// when they are. What differs between the engines is passes, not coverage, and
// SinglePass already says that.
//
// Why this exists. The engine is selected by a build tag, not by config: an
// untagged build gets the pure-Go RE2 matcher, `-tags vectorscan` gets the
// single-pass cgo one. Nothing at runtime said which, and the difference is not
// cosmetic — measured on a live compliance proxy, a 400 KB request body cost
// ~3 s of added latency on the RE2 path, with 96% of process CPU inside
// `regexp.(*machine)`, because RE2 scans every pattern over every segment
// separately while Vectorscan scans all patterns in one pass. A build that
// silently loses its cgo engine therefore keeps working, keeps producing correct
// verdicts, and gets an order of magnitude slower on large bodies — the shape of
// problem that surfaces as "the proxy feels slow" weeks later. Announcing the
// engine at boot and serving it from runtime introspection is what makes that a
// five-second question.
type Engine struct {
	// Name is "vectorscan" or "re2".
	Name string `json:"name"`
	// SinglePass reports whether one scan covers all patterns (Vectorscan) or
	// each pattern is scanned separately (RE2 — cost grows with pattern count).
	SinglePass bool `json:"singlePass"`
	// Effect states the operational consequence in one line, for an operator
	// reading a log or an introspection response rather than this source file.
	Effect string `json:"effect"`
}
