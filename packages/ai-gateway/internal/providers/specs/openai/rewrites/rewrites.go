// Per-model wire-quirk rules owned by openai.
//
// Per provider-adapter-architecture.md §3a Rule 3, OpenAI's per-model wire
// quirks live with the OpenAI adapter, never in the generic dispatch layer.
// The rules are data (codec.FieldRule) assembled into the identity codec's
// Contract: one quirk table that both codec entry points — EncodeRequest
// (canonical door) and RewriteNative (native door) — apply through one
// shared applier, and that both request wires carrying these fields at the
// body root (/v1/chat/completions and /v1/responses) draw from. A set that
// drifts between wires makes the same model answer 200 on one ingress and
// 400 on the other; a set that drifts between doors makes the same body
// behave differently depending on which leg of dispatch it took.
package rewrites

import (
	"strings"

	openaicodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/codec"
)

// NeedsMaxTokensRename reports whether modelID belongs to an OpenAI
// reasoning family whose chat-completions wire rejects the classic
// `max_tokens` parameter name.
//
// Observed (2026-05, direct calls to api.openai.com; gpt-5.4 re-probed
// 2026-07-16): gpt-5.4 / gpt-5.4-mini / gpt-5.4-nano / gpt-5.5 — 400
// "Unsupported parameter: 'max_tokens' is not supported with this model.
// Use 'max_completion_tokens' instead."; o1 / o3 / o3-mini / o4-mini —
// the same 400. The rename covers every gpt-5* family INCLUDING gpt-5.4:
// its sampling-params exemption below does not extend to max_tokens.
//
// Detection is by ID prefix because the upstream gates parameters on the
// model identifier, not on a discoverable capability flag. "o" followed by
// a digit matches future o5/o6 snapshots; any non-digit (legacy
// "openai-..." names) is rejected so non-reasoning calls are never touched.
func NeedsMaxTokensRename(modelID string) bool {
	return strings.HasPrefix(modelID, "gpt-5") || isOSeries(modelID)
}

// RejectsSamplingParams reports whether modelID belongs to an OpenAI
// family whose wire rejects caller-supplied sampling parameters
// (temperature / top_p) with HTTP 400, on both request wires that carry
// them at the body root.
//
// Observed 400s (api.openai.com, probed 2026-07-16): gpt-5.5 and
// gpt-5.6-luna/-sol/-terra — chat 400 "Unsupported value: 'temperature'
// does not support 0 with this model. Only the default (1) value is
// supported." and /v1/responses 400 "Unsupported parameter: 'temperature'
// is not supported with this model."; o1 / o3 / o3-mini / o4-mini — the
// same rejection class.
//
// gpt-5.4 is carved out: probed 2026-07-16 (api.openai.com, gpt-5.4 and
// gpt-5.4-mini) — temperature=0 answers 200 on BOTH wires; that family's
// only real quirk is the max_tokens rename above. The carve-out is an
// exemption from a default-strip family rule on purpose: an unprobed new
// gpt-5.x family fails safe — stripped, visible via x-nexus-coerced, no
// 400 risk — until a probe proves it accepts. Same allowlist direction as
// the anthropic codec's accepts list.
func RejectsSamplingParams(modelID string) bool {
	if hasFamilyPrefix(modelID, "gpt-5.4") {
		return false
	}
	return strings.HasPrefix(modelID, "gpt-5") || isOSeries(modelID)
}

// hasFamilyPrefix matches a model family name at a version boundary: the
// id is the family itself or continues with a separator ('-' or '.').
// A bare HasPrefix would let a hypothetical "gpt-5.40" inherit
// "gpt-5.4"'s probed ACCEPT carve-out (the fail-unsafe direction) and
// "gpt-5.60" inherit gpt-5.6's forced effort.
func hasFamilyPrefix(modelID, family string) bool {
	if !strings.HasPrefix(modelID, family) {
		return false
	}
	if len(modelID) == len(family) {
		return true
	}
	next := modelID[len(family)]
	return next == '-' || next == '.'
}

// isOSeries matches "o" followed by a digit (o1, o3, o4-mini, future o5+
// snapshots). Any non-digit second byte is rejected so legacy
// "openai-..." style names never match.
func isOSeries(modelID string) bool {
	return len(modelID) >= 2 && modelID[0] == 'o' && modelID[1] >= '1' && modelID[1] <= '9'
}

// RequiresEffortNoneWithTools reports whether modelID belongs to an
// OpenAI family whose /v1/chat/completions wire rejects function tools
// unless reasoning_effort is EXPLICITLY "none".
//
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
	return hasFamilyPrefix(modelID, "gpt-5.6")
}

// IsAda002Embedding reports whether modelID is the text-embedding-ada-002
// family, which rejects request fields newer embedding models accept —
// observed 400 "Unrecognized request argument supplied: dimensions"
// (reproduced against api.openai.com with model=text-embedding-ada-002).
func IsAda002Embedding(modelID string) bool {
	return strings.HasPrefix(modelID, "text-embedding-ada-002")
}

// The quirk table. Rule order fixes the order rewrites are reported in
// (and therefore the x-nexus-coerced header contents).
var (
	// Chat-wire rename; evidence on NeedsMaxTokensRename above.
	maxTokensRename = openaicodec.FieldRule{
		Applies:  NeedsMaxTokensRename,
		Field:    "max_tokens",
		RenameTo: "max_completion_tokens",
	}
	// Sampling strips; evidence on RejectsSamplingParams above. Shared by
	// the chat and responses rule lists so the two wires cannot drift.
	temperatureStrip = openaicodec.FieldRule{
		Applies: RejectsSamplingParams,
		Field:   "temperature",
	}
	topPStrip = openaicodec.FieldRule{
		Applies: RejectsSamplingParams,
		Field:   "top_p",
	}
	// Chat-wire forced value; evidence on RequiresEffortNoneWithTools
	// above. The condition gate keeps tool-less traffic untouched (the
	// family's reasoning default is only rejected when function tools are
	// in the body), and "none" — not removal — is the only accepted
	// state, because the ABSENT field 400s too.
	effortNoneWithTools = openaicodec.FieldRule{
		Applies:             RequiresEffortNoneWithTools,
		Field:               "reasoning_effort",
		SetRaw:              `"none"`,
		WhenPresentNonEmpty: "tools",
		Label:               "reasoning_effort→none (function tools on the chat wire)",
	}
	// Embeddings strips; evidence on IsAda002Embedding above. The labels
	// carry the family so x-nexus-coerced explains the strip.
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
)

// OpenAIContract assembles the OpenAI wire contract. Azure OpenAI shares
// it verbatim: Azure serves the same model families behind deployment
// URLs and returns the same wire 400s, so the two adapters construct
// their identity codecs from this one table.
//
// The responses list carries the sampling strips but NOT the max_tokens
// rename on purpose: /v1/responses carries the output cap as
// max_output_tokens, so BOTH chat-wire names are invalid there and
// renaming one rejected parameter to another only changes which 400 the
// caller gets. Only the sampling strips have an observed /v1/responses
// 400 behind them.
func OpenAIContract() openaicodec.Contract {
	return openaicodec.Contract{
		Chat:       []openaicodec.FieldRule{maxTokensRename, temperatureStrip, topPStrip, effortNoneWithTools},
		Responses:  []openaicodec.FieldRule{temperatureStrip, topPStrip},
		Embeddings: []openaicodec.FieldRule{ada002DimensionsStrip, ada002EncodingFormatStrip},
	}
}
