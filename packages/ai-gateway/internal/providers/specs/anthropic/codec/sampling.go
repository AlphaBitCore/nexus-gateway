package codec

import "strings"

// Per-model sampling-parameter policy for the Anthropic wire: which Claude
// families accept temperature / top_p / top_k, which reject the
// temperature+top_p combination, and the version-boundary matcher that keeps
// the accepting allowlist from leaking to look-alike future families. Applied
// by EncodeRequest (cross-format leg) and applyNativeSamplingPolicy (native
// leg) alike, so both doors coerce identically.

// claudeModelsAcceptingSamplingParams lists the Claude family prefixes that
// still accept temperature / top_p / top_k. Anthropic retires these params
// generation by generation: every family from Opus 4.7 onward rejects each
// of them on its own with 400 "`temperature` is deprecated for this model."
//
// This is an ALLOWLIST, and the direction is the point. A denylist of
// rejecting families fails the wrong way: an unlisted model — which every
// future Claude release starts out as — has the params forwarded and 400s
// every SDK client that sets temperature by default, surfacing only as a
// production incident. The allowlist inverts that: an unlisted model has
// them stripped and its request still succeeds, losing only sampling
// control, which the emitted rewrite makes visible via the x-nexus-coerced
// response header.
//
// Family matching (hasFamilyPrefix) covers the dated variants Anthropic ships
// (claude-opus-4-5 also matches claude-opus-4-5-20251101). Populate this
// list only from a direct probe against api.anthropic.com: the vendor's own
// deprecation table under-reports the rejecting set, omitting
// claude-fable-5, which rejects.
var claudeModelsAcceptingSamplingParams = []string{
	"claude-opus-4-6",
	"claude-opus-4-5",
	"claude-opus-4-1",
	"claude-sonnet-4-6",
	"claude-sonnet-4-5",
	"claude-haiku-4-5",
	// The 3.x, 2.x and instant families accept the sampling params. They are
	// retired on api.anthropic.com and so no longer directly probeable, but
	// they stay reachable through a provider pointed at a partner platform or
	// a proxy, and they honour the params they are sent — stripping them there
	// would forfeit caller intent (e.g. a `temperature: 0` request for
	// deterministic output) without averting any 400. Same reasoning as the
	// 3.x carve-out: the allowlist must not conflate known-older-accepting
	// families with the unknown-future families it fails safe toward.
	"claude-3",
	"claude-2",
	"claude-instant",
}

// anthropicModelRejectsSamplingParams reports whether temperature / top_p /
// top_k must be stripped before the body reaches the wire.
//
// Only Claude models are judged. A provider configured with the anthropic
// adapter but a non-Claude model id is some other endpoint speaking the
// Messages shape; we have never probed it and must not silently strip
// sampling params it may well honour.
func anthropicModelRejectsSamplingParams(model string) bool {
	if !strings.HasPrefix(model, "claude-") {
		return false
	}
	for _, prefix := range claudeModelsAcceptingSamplingParams {
		if hasFamilyPrefix(model, prefix) {
			return false
		}
	}
	return true
}

// hasFamilyPrefix matches a family name at a version boundary: after the prefix
// the model id must have a `-`, a `.`, or nothing at all. A bare strings.HasPrefix
// would let a hypothetical future "claude-20-..." inherit "claude-2"'s
// accepts-sampling carve-out — the fail-UNSAFE direction, since a rejecting
// family would then have its params forwarded and 400 (the exact incident the
// allowlist prevents). The dated variants Anthropic ships stay matched
// (claude-opus-4-6 → claude-opus-4-6-20251101, boundary '-'). Mirrors the
// OpenAI codec's hasFamilyPrefix.
func hasFamilyPrefix(model, family string) bool {
	if !strings.HasPrefix(model, family) {
		return false
	}
	if len(model) == len(family) {
		return true
	}
	switch model[len(family)] {
	case '-', '.':
		return true
	}
	return false
}

// anthropicModelRejectsTempTopPTogether reports whether the model
// belongs to a family that ACCEPTS temperature or top_p alone but
// REJECTS the combination with 400 "`temperature` and `top_p` cannot
// both be specified for this model." When true, the codec drops top_p
// when temperature is also present (temperature is kept because the
// OpenAI SDK default is to set it, while top_p is usually an
// intentional advanced override).
//
// Every claude-4.x family probed against api.anthropic.com rejects the
// combination. This stays a denylist, unlike the sampling-param
// allowlist above: rejecting the combination is a second, independent
// vendor behaviour, and folding the two lists together would make
// admitting a model to the allowlist silently assert a claim about the
// combination that no probe had established.
//
// Only families that accept the params alone reach this check — the
// rest are stripped wholesale by anthropicModelRejectsSamplingParams.
func anthropicModelRejectsTempTopPTogether(model string) bool {
	switch {
	case strings.HasPrefix(model, "claude-haiku-4-"),
		strings.HasPrefix(model, "claude-sonnet-4-"),
		strings.HasPrefix(model, "claude-opus-4-"):
		return true
	}
	return false
}
