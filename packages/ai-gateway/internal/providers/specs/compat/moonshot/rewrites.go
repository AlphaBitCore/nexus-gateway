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
)

// IsFixedTempModel reports whether the Moonshot model id belongs to a
// family that hardcodes temperature on the upstream side and rejects
// any caller-supplied value with HTTP 400 "invalid temperature: only
// 1 is allowed for this model." Older kimi-k2-thinking and
// moonshot-v1-* models accept arbitrary temperature.
//
// Observed, each by sending temperature to that model and reading the
// upstream's own 400 back: kimi-k2.5, kimi-k2.6, kimi-k2.7-code,
// kimi-k2.7-code-highspeed. Each answers 200 to the same request with
// temperature omitted, so the family, not the request, is the cause.
//
// This list is a denylist, and a denylist over a catalog that gains
// models without a code change is a list that goes stale silently: the
// k2.7 families shipped in the catalog and 400'd every
// temperature-sending client until a smoke run caught them. When a
// Moonshot family appears, send it a temperature before assuming it
// accepts one (the quirk-coverage lint forces the recorded decision).
func IsFixedTempModel(modelID string) bool {
	switch {
	case strings.HasPrefix(modelID, "kimi-k2.5"),
		strings.HasPrefix(modelID, "kimi-k2.6"),
		strings.HasPrefix(modelID, "kimi-k2.7"):
		return true
	}
	return false
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
