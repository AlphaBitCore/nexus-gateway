// Per-model wire-quirk rules owned by moonshot.
//
// Per provider-adapter-architecture.md §3a Rule 3, Moonshot's per-model
// wire quirks (kimi-k2.5 / k2.6 / k2.7 require temperature=1 and reject
// any other value with HTTP 400) live with the Moonshot adapter, not in
// the generic dispatch layer. The rules are data (codec.FieldRule)
// assembled into the identity codec's Contract, so both codec entry
// points — the cross-format canonical door and the native-leg
// differential — apply them identically: the historical gap was exactly
// a strip that fired on the OpenAI-chat ingress but not on bodies bridged
// from /v1/messages, which 400'd on the same models.
package moonshot

import (
	"strings"

	openaicodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/codec"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

// kimiFamiliesAcceptingTemperature lists the ids inside Moonshot's `kimi-`
// namespace that a live probe has shown to accept a caller-supplied
// temperature. Everything else under `kimi-` is treated as fixed-temp.
//
// Membership requires an observed 200 with a temperature in the body.
// Absence costs a stripped parameter (recorded on x-nexus-coerced, the
// upstream applies its mandatory =1); presence costs a hard 400. The
// asymmetry is why this is an ACCEPTS list and not a denylist.
var kimiFamiliesAcceptingTemperature = []string{
	// Probed: accepts arbitrary temperature.
	"kimi-k2-thinking",
}

// IsFixedTempModel reports whether the Moonshot model id belongs to a
// family that hardcodes temperature on the upstream side and rejects
// any caller-supplied value with HTTP 400 "invalid temperature: only
// 1 is allowed for this model."
//
// Observed, each by sending temperature to that model and reading the
// upstream's own 400 back: kimi-k2.5, kimi-k2.6, kimi-k2.7-code,
// kimi-k2.7-code-highspeed. Each answers 200 to the same request with
// temperature omitted, so the family, not the request, is the cause.
//
// This was a denylist of those four, and a denylist over a catalog that
// gains models without a code change goes stale silently: the k2.7
// families shipped in the catalog and 400'd every temperature-sending
// client until a smoke run caught them. Inverting it makes the stale
// direction harmless — a `kimi-` id nobody has probed (kimi-k3 is one, as
// of 2026-08-06 it is in the vendor's /v1/models and not in our catalog)
// is stripped rather than forwarded on a guess. `moonshot-v1-*` sits
// outside the namespace and keeps its caller's temperature untouched.
//
// Same shape as the anthropic codec's accepts list, for the same reason.
func IsFixedTempModel(modelID string) bool {
	if !strings.HasPrefix(modelID, "kimi-") {
		return false
	}
	for _, family := range kimiFamiliesAcceptingTemperature {
		if specutil.MatchesFamily(modelID, family) {
			return false
		}
	}
	return true
}

// Contract assembles the Moonshot wire contract: the fixed-temp families
// lose the caller's temperature (and its companion top_p) so the upstream
// applies its mandatory =1 default — sending any other value hard-fails
// the request. Rule order fixes the x-nexus-coerced report order.
func Contract() openaicodec.Contract {
	return openaicodec.Contract{
		Chat: []openaicodec.FieldRule{
			{Applies: IsFixedTempModel, Field: "temperature"},
			{Applies: IsFixedTempModel, Field: "top_p"},
		},
	}
}
