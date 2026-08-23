// Per-model wire-quirk rules owned by openai (architecture §3a Rule 3).
//
// One table, applied by both codec doors (EncodeRequest and RewriteNative) and
// drawn from by both root-level request wires: a set that drifts between wires
// makes the same model answer 200 on one ingress and 400 on the other.
package rewrites

import (
	"strings"

	openaicodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/codec"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

// Observed (2026-05, direct calls to api.openai.com; gpt-5.4 re-probed
// 2026-07-16): gpt-5.4 / gpt-5.4-mini / gpt-5.4-nano / gpt-5.5 — 400
// "Unsupported parameter: 'max_tokens' is not supported with this model.
// Use 'max_completion_tokens' instead."; o1 / o3 / o3-mini / o4-mini — the
// same 400. Covers gpt-5.4 too: its sampling-params exemption below does not
// extend to max_tokens.
//
// Detection is by ID because the upstream gates on the model identifier, not
// on a discoverable capability flag. The generation test is >= 5, not a
// literal "gpt-5" prefix, so an unreleased gpt-6 lands inside the rule.
func NeedsMaxTokensRename(modelID string) bool {
	return specutil.GenerationAtLeast(modelID, "gpt-", 5) || isOSeries(modelID)
}

// Observed 400s (api.openai.com, probed 2026-07-16): gpt-5.5 and
// gpt-5.6-luna/-sol/-terra — chat 400 "Unsupported value: 'temperature'
// does not support 0 with this model. Only the default (1) value is
// supported." and /v1/responses 400 "Unsupported parameter: 'temperature'
// is not supported with this model."; o1 / o3 / o3-mini / o4-mini — the
// same rejection class.
//
// gpt-5.4 is carved out: probed 2026-07-16 (gpt-5.4 and gpt-5.4-mini),
// temperature=0 answers 200 on BOTH wires. The carve-out is an exemption from
// a default-strip family rule on purpose, so an unprobed new gpt-5.x fails
// safe — stripped and visible via x-nexus-coerced — until a probe proves it
// accepts.
//
// The rejecting set is every generation from 5 up, not the "gpt-5" string: a
// literal prefix leaves gpt-6 outside, where the upstream decides — which is
// how kimi-k2.7 reached callers as a 400 (see compat/moonshot). gpt-4o and
// earlier stay outside; they accept sampling.
func RejectsSamplingParams(modelID string) bool {
	for _, family := range gptFamiliesAcceptingSamplingParams {
		if specutil.MatchesFamily(modelID, family) {
			return false
		}
	}
	return specutil.GenerationAtLeast(modelID, "gpt-", 5) || isOSeries(modelID)
}

// Membership requires an observed 200: absence costs a stripped parameter,
// presence costs a 400.
var gptFamiliesAcceptingSamplingParams = []string{
	// Probed 2026-07-16 (api.openai.com): gpt-5.4 and gpt-5.4-mini answer
	// 200 to temperature=0 on both the chat and responses wires.
	"gpt-5.4",
}

// "o" followed by a digit (o1, o3, o4-mini, future o5+). A non-digit second
// byte is rejected so legacy "openai-..." names never match.
func isOSeries(modelID string) bool {
	return len(modelID) >= 2 && modelID[0] == 'o' && modelID[1] >= '1' && modelID[1] <= '9'
}

// Observed (2026-07-17, api.openai.com via live boundary probes; first
// seen as a prod traffic_event 400): gpt-5.6-terra and gpt-5.6-luna with
// a non-empty tools[] answer 400 "Function tools with reasoning_effort
// are not supported for gpt-5.6-terra in /v1/chat/completions. To use
// function tools, use /v1/responses or set reasoning_effort to 'none'."
// — for reasoning_effort "high", "low", AND when the field is ABSENT
// (the family default is itself rejected, so stripping the field cannot
// fix the request; only the explicit "none" does — probed 200 with
// working tool_calls). tools:[] (empty array) answers 200 without the
// field. gpt-5.5 and gpt-5.4 accept tools with the field absent (probed
// 200 same day), so the rule is gpt-5.6-only, not family-wide gpt-5*.
func RequiresEffortNoneWithTools(modelID string) bool {
	return specutil.MatchesFamily(modelID, "gpt-5.6")
}

// ada-002 rejects request fields newer embedding models accept — observed 400
// "Unrecognized request argument supplied: dimensions" (reproduced against
// api.openai.com with model=text-embedding-ada-002).
func IsAda002Embedding(modelID string) bool {
	return strings.HasPrefix(modelID, "text-embedding-ada-002")
}

// Rule order fixes the order rewrites are reported in, and therefore the
// x-nexus-coerced header contents.
var (
	maxTokensRename = openaicodec.FieldRule{
		Applies:  NeedsMaxTokensRename,
		Field:    "max_tokens",
		RenameTo: "max_completion_tokens",
	}
	// Shared by the chat and responses rule lists so the two cannot drift.
	temperatureStrip = openaicodec.FieldRule{
		Applies: RejectsSamplingParams,
		Field:   "temperature",
	}
	topPStrip = openaicodec.FieldRule{
		Applies: RejectsSamplingParams,
		Field:   "top_p",
	}
	// The condition gate keeps tool-less traffic untouched, and "none" —
	// not removal — is the only accepted state, because the ABSENT field
	// 400s too.
	effortNoneWithTools = openaicodec.FieldRule{
		Applies:             RequiresEffortNoneWithTools,
		Field:               "reasoning_effort",
		SetRaw:              `"none"`,
		WhenPresentNonEmpty: "tools",
		Label:               "reasoning_effort→none (function tools on the chat wire)",
	}
	ada002DimensionsStrip = openaicodec.FieldRule{
		Applies: IsAda002Embedding,
		Field:   "dimensions",
		Label:   "dimensions→removed (ada-002: unsupported field)",
	}
	ada002EncodingFormatStrip = openaicodec.FieldRule{
		Applies: IsAda002Embedding,
		Field:   "encoding_format",
		Label:   "encoding_format→removed (ada-002: unsupported field)",
	}
	// modalities is valid on this wire — the audio models REQUIRE it — and a
	// hard 400 on every other model. Evidence on RejectsModalities below.
	modalitiesStrip = openaicodec.FieldRule{
		Applies: RejectsModalities,
		Field:   "modalities",
		Label:   "modalities→removed (model does not accept the field)",
	}
)

// A caller may legitimately send `modalities`: OpenAI's audio models require it.
//
// Observed by replaying 400 production 4xx with their original payloads against
// the live gateway: 17 came back "unknown_parameter Unknown parameter:
// 'modalities'." — on gpt-5.4, gpt-5.4-mini, gpt-5.4-nano, gpt-5.5,
// gpt-5.6-luna, gpt-5.6-sol, gpt-5.6-terra, o1, o3, o3-mini and o4-mini.
//
// Audio models are excluded EXPLICITLY rather than left to the generation test:
// stripping the field there turns a working request into "This model requires
// that either input content or output modality contain audio".
func RejectsModalities(modelID string) bool {
	// Substring, not a prefix: a prefix catches today's "gpt-audio-*" and
	// misses a future "gpt-6-audio", which WOULD match the generation test
	// below and lose the one field it cannot run without.
	if strings.Contains(modelID, "audio") {
		return false
	}
	return specutil.GenerationAtLeast(modelID, "gpt-", 5) || isOSeries(modelID)
}

// Azure OpenAI shares this verbatim: same model families, same wire 400s.
//
// The responses list carries the sampling strips but NOT the max_tokens
// rename: /v1/responses names the output cap max_output_tokens, so both
// chat-wire names are invalid there and renaming one rejected parameter to
// another only changes which 400 the caller gets.
func OpenAIContract() openaicodec.Contract {
	return openaicodec.Contract{
		Chat:       []openaicodec.FieldRule{maxTokensRename, temperatureStrip, topPStrip, effortNoneWithTools, modalitiesStrip},
		Responses:  []openaicodec.FieldRule{temperatureStrip, topPStrip},
		Embeddings: []openaicodec.FieldRule{ada002DimensionsStrip, ada002EncodingFormatStrip},
	}
}
