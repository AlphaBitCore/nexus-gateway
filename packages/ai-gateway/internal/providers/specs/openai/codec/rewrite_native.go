package codec

import (
	"fmt"

	"github.com/goccy/go-json"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// clampChatMaxTokens caps an OpenAI-chat body's max_tokens / max_completion_tokens
// to the model's advertised output ceiling when the caller set a value OVER it.
// The upstream otherwise hard-400s ("max_tokens is too large: N. This model
// supports at most M ..."), and the request was failing regardless — a request
// asking for more output than the model can produce cannot be honoured, so
// clamping to the ceiling is the only success path, made visible via
// x-nexus-coerced. This is the same capability-fill discipline Anthropic's codec
// applies (clampMaxTokens); it is generic — clamp to the model's own limit, not a
// per-model quirk — so it needs no prefix list.
//
// Only an EXISTING value is clamped: OpenAI treats max_tokens as OPTIONAL, so an
// absent field is left absent (never filled — that would change the caller's
// unbounded-output intent). Both fields are checked independently; the upstream
// resolves precedence. limit<=0 (unknown ceiling) is a no-op so a missing
// capability value never coerces to a wrong number.
func clampChatMaxTokens(body []byte, limit int) ([]byte, []string) {
	if limit <= 0 {
		return body, nil
	}
	var rewrites []string
	out := body
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		v := gjson.GetBytes(out, field)
		if !v.Exists() {
			continue
		}
		if v.Int() > int64(limit) {
			out, _ = sjson.SetBytes(out, field, limit)
			rewrites = append(rewrites, fmt.Sprintf("%s→%d_model_max", field, limit))
		}
	}
	return out, rewrites
}

// RewriteNative applies the OpenAI-wire same-spec differential: the diff a
// canonical round-trip would have applied to a body that is already in the
// OpenAI shape. Every wire arm delegates to the same per-wire body its
// EncodeRequest counterpart uses, so an aliased model and the contract's
// per-model rules behave identically whichever entry point dispatch took.
//
//   - chat, non-stream: contract rules (surgical; decode door on dup keys)
//   - the resolved-model stamp.
//   - chat, streaming: ensure `stream: true` and
//     `stream_options.include_usage` so OpenAI-compatible upstreams emit
//     the final usage chunk — without it the audit row degrades to
//     usage_extraction_status=streaming_unavailable. A body that already
//     conforms — model, stream, include_usage present, no contract rule
//     due — forwards verbatim off fused probes.
//   - responses: stamp + the contract's responses rules via
//     encodeResponsesNative, the one body behind both of that wire's
//     entry points.
//   - embeddings: stamp + the contract's embeddings rules. The canonical
//     door's wire-contract validation does NOT run here: the native door
//     forwards what it does not rewrite and lets the upstream be the
//     validator (wire must equal audit).
//   - legacy completions: model stamp only — max_tokens is the correct
//     parameter name on that wire and no rule targets it.
//   - anything else: forwarded verbatim (no body-root model on that wire).
func (c *identityCodec) RewriteNative(shape typology.WireShape, nativeBody []byte, target provcore.CallTarget, stream bool) (provcore.EncodeResult, error) {
	switch shape {
	case typology.WireShapeOpenAIResponses:
		return c.encodeResponsesNative(nativeBody, target)
	case typology.WireShapeOpenAIChat:
		if stream {
			return c.rewriteStreamingChat(nativeBody, target)
		}
		return c.chatDifferential(nativeBody, target)
	case typology.WireShapeOpenAIEmbeddings:
		stamped, err := provcore.SurgicalModelStamp(nativeBody, target.ProviderModelID)
		if err != nil {
			return provcore.EncodeResult{}, err
		}
		out, rewrites, ok := c.embeddings.applyBytes(stamped, target.ProviderModelID)
		if !ok {
			return c.mapDoor(nativeBody, target, &c.embeddings, false)
		}
		return provcore.EncodeResult{Body: out, ContentType: "application/json", Rewrites: rewrites}, nil
	case typology.WireShapeOpenAICompletionsLegacy,
		typology.WireShapeOpenAIImages, typology.WireShapeOpenAIAudioSpeech:
		// The multimodal OpenAI JSON wires carry `model` at the body root
		// (/v1/images/generations, /v1/audio/speech) exactly like chat, so a
		// same-format request must stamp the resolved provider model over the
		// client-facing alias — otherwise the upstream 404s. No per-model
		// content rules apply (native passthrough beyond the model stamp).
		out, err := provcore.SurgicalModelStamp(nativeBody, target.ProviderModelID)
		if err != nil {
			return provcore.EncodeResult{}, err
		}
		return provcore.EncodeResult{Body: out, ContentType: "application/json"}, nil
	default:
		return provcore.EncodeResult{Body: nativeBody, ContentType: "application/json"}, nil
	}
}

// chatDifferential is the one body behind both non-streaming chat doors:
// EncodeRequest's chat arm and RewriteNative's non-stream chat arm. It
// applies the contract's chat rules surgically, then the resolved-model
// stamp. The fast paths cost what they did before contracts existed: a
// model with no gate-matching rule skips the probe entirely (zero body
// reads beyond the stamp's own), and a gate-matching model with nothing
// due pays one fused presence walk.
func (c *identityCodec) chatDifferential(body []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	res, err := c.chatDifferentialBody(body, target)
	return withMaxTokensClamp(res, err, target)
}

func (c *identityCodec) chatDifferentialBody(body []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	// A gate-matching structural rule needs the parsed form — route to
	// the decode door up front (this is what the transitional dispatch
	// callback cost for the same models; non-matching models skip it
	// entirely).
	if c.structuralDue(target.ProviderModelID) {
		return c.mapDoor(body, target, &c.chat, false)
	}
	out, rewrites, ok := c.chat.applyBytes(body, target.ProviderModelID)
	if !ok {
		return c.mapDoor(body, target, &c.chat, false)
	}
	stamped, err := provcore.SurgicalModelStamp(out, target.ProviderModelID)
	if err != nil {
		return provcore.EncodeResult{}, err
	}
	return provcore.EncodeResult{Body: stamped, ContentType: "application/json", Rewrites: rewrites}, nil
}

// withMaxTokensClamp applies the model-ceiling clamp to a chat EncodeResult.
// Shared by both chat doors (canonical EncodeRequest via chatDifferential, and
// native RewriteNative via chatDifferential / rewriteStreamingChat) so an
// over-ceiling max_tokens is coerced identically whichever entry point ran.
// A pass-through on error keeps the codec's error contract unchanged.
func withMaxTokensClamp(res provcore.EncodeResult, err error, target provcore.CallTarget) (provcore.EncodeResult, error) {
	if err != nil {
		return res, err
	}
	clamped, rw := clampChatMaxTokens(res.Body, target.MaxOutputTokens)
	if len(rw) > 0 {
		res.Body = clamped
		res.Rewrites = append(res.Rewrites, rw...)
	}
	return res, nil
}

// structuralDue reports whether any structural rule gate-matches — a
// model-id check only, zero body reads.
func (c *identityCodec) structuralDue(modelID string) bool {
	for _, r := range c.chatStructural {
		if r.Applies(modelID) {
			return true
		}
	}
	return false
}

// rewriteStreamingChat is the streaming arm of the chat differential. The
// all-skip probe answers "is anything due at all?" — a conformant body
// (provider model already set, stream:true, include_usage present, no
// contract rule field present for a gate-matching model) forwards
// verbatim with zero allocation. Otherwise one map round-trip applies the
// stamp, the contract rules, and the usage option together (a benchmark
// pinned map[string]any as the lowest-alloc round-trip when a rewrite IS
// needed; surgical chaining allocates ~2× the bytes at 3+ edits, and the
// streaming back-fills need the decoded form anyway).
func (c *identityCodec) rewriteStreamingChat(body []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	res, err := c.rewriteStreamingChatBody(body, target)
	return withMaxTokensClamp(res, err, target)
}

func (c *identityCodec) rewriteStreamingChatBody(body []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	if len(body) == 0 {
		return provcore.EncodeResult{Body: body, ContentType: "application/json"}, nil
	}
	if !c.structuralDue(target.ProviderModelID) {
		r := gjson.GetManyBytes(body, "model", "stream", "stream_options.include_usage")
		if (target.ProviderModelID == "" || r[0].String() == target.ProviderModelID) && r[1].Exists() && r[2].Exists() &&
			!c.chat.anyFieldPresent(body, target.ProviderModelID) {
			return provcore.EncodeResult{Body: body, ContentType: "application/json"}, nil
		}
	}
	return c.mapDoor(body, target, &c.chat, true)
}

// mapDoor is the decode door shared by every wire arm: one map round-trip
// that applies the model stamp, the given rule set, and (when streaming)
// the usage-option injection together. It serves the paths that must
// decode anyway — a duplicated key ahead of a surgical edit, or a
// streaming body that needs back-fills — where map edits are exact (the
// decode de-duplicates keys) and cost nothing extra.
func (c *identityCodec) mapDoor(body []byte, target provcore.CallTarget, rules *ruleSet, stream bool) (provcore.EncodeResult, error) {
	return c.mapDoorGated(body, target, rules, stream, target.ProviderModelID)
}

// mapDoorGated is mapDoor with an explicit rule-gate model id: the
// embeddings canonical door resolves its gate from the body when the
// target carries no ProviderModelID, and the decode fallback must gate on
// the same id or the two doors diverge on that (degenerate) input.
func (c *identityCodec) mapDoorGated(body []byte, target provcore.CallTarget, rules *ruleSet, stream bool, gateModelID string) (provcore.EncodeResult, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return provcore.EncodeResult{}, err
	}
	if target.ProviderModelID != "" {
		payload["model"] = target.ProviderModelID
	}
	rewrites := rules.applyMap(payload, gateModelID)
	// Structural rules ride the chat wire only; the chat rule set is the
	// marker for "this is the chat decode door".
	if rules == &c.chat {
		for _, sr := range c.chatStructural {
			if sr.Applies(gateModelID) {
				rewrites = append(rewrites, sr.Apply(payload, gateModelID)...)
			}
		}
	}
	if stream {
		ensureStreamUsage(payload)
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return provcore.EncodeResult{}, err
	}
	return provcore.EncodeResult{Body: out, ContentType: "application/json", Rewrites: rewrites}, nil
}

// ensureStreamUsage sets `stream: true` (cross-format ingresses signal
// streaming out-of-band and their canonical bodies may omit it — adding
// stream_options without stream enabled is an upstream 400) and
// stream_options.include_usage when the caller has not already set it.
func ensureStreamUsage(payload map[string]any) {
	if _, ok := payload["stream"]; !ok {
		payload["stream"] = true
	}
	// A present-but-non-object stream_options is replaced wholesale: the
	// upstream would reject it anyway, and usage extraction needs the
	// include_usage flag to survive.
	opts, _ := payload["stream_options"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
		payload["stream_options"] = opts
	}
	if _, ok := opts["include_usage"]; !ok {
		opts["include_usage"] = true
	}
}

// encodeResponsesNative is the one body behind both doors of the
// /v1/responses wire: EncodeRequest's responses arm and RewriteNative both
// call it, so an aliased model and the contract's responses rules behave
// identically whichever entry point the dispatch layer used.
func (c *identityCodec) encodeResponsesNative(body []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	out, err := provcore.SurgicalModelStamp(body, target.ProviderModelID)
	if err != nil {
		return provcore.EncodeResult{}, err
	}
	out, rewrites, ok := c.responses.applyBytes(out, target.ProviderModelID)
	if !ok {
		return c.mapDoor(body, target, &c.responses, false)
	}
	return provcore.EncodeResult{Body: out, ContentType: "application/json", Rewrites: rewrites}, nil
}
