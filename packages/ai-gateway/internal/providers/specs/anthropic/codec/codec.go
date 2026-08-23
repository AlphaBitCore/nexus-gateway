package codec

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	normcodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// errUnsupportedField returns the canonical structured error codecs MUST
// surface when a request field cannot be expressed in the target wire
// format. Callers wrap this directly into the response pipeline so the
// client sees a 400 with a stable error type, never a silent drop.
func errUnsupportedField(field string) error {
	return &provcore.ProviderError{
		Status:  http.StatusBadRequest,
		Code:    provcore.CodeInvalidRequest,
		Type:    "nexus_field_unsupported",
		Message: "nexus: field " + field + " unsupported on this route",
	}
}

// Codec translates OpenAI `/v1/chat/completions` bodies to and from
// the Anthropic Messages shape. Scope is intentionally the subset used
// by the gateway today:
//   - single system prompt → top-level "system" string
//   - user/assistant messages → "messages" array with text content
//   - temperature / top_p / max_tokens / stream fields
//   - stop sequences → "stop_sequences"
//
// A native Anthropic body (BodyFormat = anthropic) skips the canonical
// round-trip and takes the codec's RewriteNative differential instead — the
// codec stays in the path (dispatch calls RewriteNative on the native leg).
// Tool calling, image parts, and thinking mode ride through verbatim there;
// only the model stamp and the D3 sampling / max_tokens coercions apply.
type Codec struct{}

// NewCodec returns an Anthropic SchemaCodec for use by spec.go and bedrock.
func NewCodec() provcore.SchemaCodec { return Codec{} }

// EncodeRequest canonical OpenAI → native Anthropic.
func (Codec) EncodeRequest(endpoint typology.WireShape, canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	if endpoint != typology.WireShapeAnthropicMessages {
		return provcore.EncodeResult{}, fmt.Errorf("anthropic: unsupported endpoint %q for codec", endpoint)
	}
	if len(canonicalBody) == 0 {
		return provcore.EncodeResult{}, fmt.Errorf("anthropic: empty canonical body")
	}
	root := gjson.ParseBytes(canonicalBody)

	model := root.Get("model").String()
	if target.ProviderModelID != "" {
		model = target.ProviderModelID
	}
	if model == "" {
		return provcore.EncodeResult{}, fmt.Errorf("anthropic: missing model")
	}

	// Per-model sampling-param policy. Two distinct rules apply:
	//
	//  (a) Families that reject temperature / top_p / top_k outright —
	//      any one of them present → 400 "`temperature` is deprecated
	//      for this model." Strip all three. Membership is decided by
	//      allowlist, so an unrecognised model lands here and degrades
	//      to a working request rather than a 400; see
	//      claudeModelsAcceptingSamplingParams.
	//
	//  (b) The families that still accept them take EITHER temperature
	//      OR top_p but reject the combination with 400 "`temperature`
	//      and `top_p` cannot both be specified for this model." When
	//      the caller sent both, keep temperature (the OpenAI-SDK
	//      default that's almost always set on purpose) and drop top_p.
	//      top_k is independent and stays.
	//
	// Both rules emit rewrites so the handler stamps x-nexus-coerced
	// for caller observability — mirrors spec_adapter.applyOpenAIReasoningRewrites.
	rejectsSampling := anthropicModelRejectsSamplingParams(model)
	coexistsTopPWithTemp := !rejectsSampling && anthropicModelRejectsTempTopPTogether(model) &&
		root.Get("temperature").Exists() && root.Get("top_p").Exists()
	var rewrites []string

	out := map[string]any{"model": model}

	// Anthropic is the one supported wire that REQUIRES max_tokens (verified
	// against api.anthropic.com: omitting it returns 400 "max_tokens: Field
	// required", while OpenAI / Gemini / DeepSeek all accept its absence), so
	// this codec must always emit one.
	//
	// The ceiling comes from the catalog capability on the resolved target —
	// the same maxOutputTokens the gateway advertises on /v1/models. Keeping a
	// private per-family table here would restate a fact the catalog already
	// owns, and the two copies drift: a caller that trusts an advertised
	// ceiling and echoes it back must never be rejected by the very cap we
	// published.
	//
	// max_completion_tokens (OpenAI 2024-09 successor to max_tokens for
	// reasoning models) takes precedence over max_tokens when both are
	// present, matching OpenAI's own resolution.
	limit := target.MaxOutputTokens
	switch {
	case root.Get("max_completion_tokens").Exists():
		out["max_tokens"] = clampMaxTokens(root.Get("max_completion_tokens").Int(), limit, &rewrites)
	case root.Get("max_tokens").Exists():
		out["max_tokens"] = clampMaxTokens(root.Get("max_tokens").Int(), limit, &rewrites)
	default:
		// The caller omitted it (legal on the OpenAI shape they came from), so
		// fill the model's full ceiling rather than a fixed floor that would
		// truncate every long response. Recorded as a rewrite so the handler
		// stamps the applied cap onto the x-nexus-coerced response header.
		filled := limit
		if filled <= 0 {
			filled = anthropicFallbackMaxOutput
		}
		out["max_tokens"] = filled
		rewrites = append(rewrites, fmt.Sprintf("max_tokens→%d_model_default", filled))
	}

	if v := root.Get("temperature"); v.Exists() {
		if rejectsSampling {
			rewrites = append(rewrites, "temperature→removed")
		} else {
			out["temperature"] = v.Float()
		}
	}
	if v := root.Get("top_p"); v.Exists() {
		switch {
		case rejectsSampling:
			rewrites = append(rewrites, "top_p→removed")
		case coexistsTopPWithTemp:
			rewrites = append(rewrites, "top_p→removed_with_temperature_present")
		default:
			out["top_p"] = v.Float()
		}
	}
	if v := root.Get("top_k"); v.Exists() {
		if rejectsSampling {
			rewrites = append(rewrites, "top_k→removed")
		} else {
			out["top_k"] = v.Int()
		}
	}
	if v := root.Get("stream"); v.Exists() {
		out["stream"] = v.Bool()
	}
	if stop := root.Get("stop"); stop.Exists() {
		switch {
		case stop.IsArray():
			var list []string
			stop.ForEach(func(_, v gjson.Result) bool {
				list = append(list, v.String())
				return true
			})
			if len(list) > 0 {
				out["stop_sequences"] = list
			}
		case stop.Type == gjson.String:
			out["stop_sequences"] = []string{stop.String()}
		}
	}

	systemParts, messages, err := splitMessages(root.Get("messages"))
	if err != nil {
		return provcore.EncodeResult{}, err
	}
	if len(systemParts) == 1 {
		out["system"] = systemParts[0]
	} else if len(systemParts) > 1 {
		blocks := make([]map[string]any, 0, len(systemParts))
		for _, s := range systemParts {
			blocks = append(blocks, map[string]any{"type": "text", "text": s})
		}
		out["system"] = blocks
	}
	if len(messages) == 0 {
		return provcore.EncodeResult{}, fmt.Errorf("anthropic: no user/assistant messages")
	}
	if tools := root.Get("tools"); tools.IsArray() && len(tools.Array()) > 0 {
		var atools []map[string]any
		tools.ForEach(func(_, t gjson.Result) bool {
			if t.Get("type").String() != "function" {
				return true
			}
			fn := t.Get("function")
			name := fn.Get("name").String()
			if name == "" {
				return true
			}
			desc := fn.Get("description").String()
			var schema any
			params := fn.Get("parameters")
			if params.Exists() && params.Raw != "" {
				if err := json.Unmarshal([]byte(params.Raw), &schema); err != nil {
					schema = map[string]any{"type": "object", "properties": map[string]any{}}
				}
			} else {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			atools = append(atools, map[string]any{
				"name":         name,
				"description":  desc,
				"input_schema": schema,
			})
			return true
		})
		if len(atools) > 0 {
			out["tools"] = atools
		}
	}
	var toolChoice map[string]any
	if tc := root.Get("tool_choice"); tc.Exists() {
		switch tc.Type {
		case gjson.String:
			switch tc.String() {
			case "auto":
				toolChoice = map[string]any{"type": "auto"}
			case "none":
				toolChoice = map[string]any{"type": "none"}
			case "required":
				toolChoice = map[string]any{"type": "any"}
			}
		case gjson.JSON:
			if tc.Get("type").String() == "function" {
				name := tc.Get("function.name").String()
				if name != "" {
					toolChoice = map[string]any{"type": "tool", "name": name}
				}
			}
		}
	}
	// Anthropic encodes parallel-tool toggling as
	// tool_choice.disable_parallel_tool_use (inverted boolean), NOT a
	// top-level parallel_tool_calls field. The Anthropic API rejects /
	// silently ignores top-level parallel_tool_calls. Map only on the
	// disabling case (Anthropic default already enables parallel).
	// Doc: https://docs.anthropic.com/en/docs/agents-and-tools/tool-use/parallel-tool-use
	//
	// Gate on a non-empty out["tools"]: Anthropic returns a hard 400
	// ("tool_choice may only be specified while providing tools") for ANY
	// tool_choice on a request that carries no tools. An OpenAI SDK can
	// legitimately send parallel_tool_calls:false on a no-tools call;
	// synthesising a tool_choice from it would turn that into a 400.
	// With no tools the toggle is meaningless, so drop it.
	if _, hasTools := out["tools"]; hasTools {
		if v := root.Get("parallel_tool_calls"); v.Exists() && !v.Bool() {
			if toolChoice == nil {
				toolChoice = map[string]any{"type": "auto"}
			}
			toolChoice["disable_parallel_tool_use"] = true
		}
	}
	if toolChoice != nil {
		out["tool_choice"] = toolChoice
	}
	if md := root.Get("metadata"); md.Exists() && md.IsObject() {
		var meta map[string]any
		if err := json.Unmarshal([]byte(md.Raw), &meta); err == nil && len(meta) > 0 {
			out["metadata"] = meta
		}
	}
	applyResponseFormat(root, out, &rewrites)
	out["messages"] = messages

	// Reasoning intent, three doors, one contract table — the whole walk
	// lives beside the allowlist it consults (thinking_contract.go).
	if err := applyReasoningIntent(canonicalBody, out, model, &rewrites); err != nil {
		return provcore.EncodeResult{}, err
	}

	canonicalext.ScanUnsupported("anthropic", canonicalBody, anthropicSupportedRequestFields)

	body, err := json.Marshal(out)
	if err != nil {
		return provcore.EncodeResult{}, err
	}
	return provcore.EncodeResult{Body: body, ContentType: "application/json", Rewrites: rewrites}, nil
}

// anthropicJSONObjectInstruction is appended to the system prompt when a
// caller sends response_format:{type:"json_object"}. Anthropic's Messages
// API has no native JSON mode; this instruction is the gateway's
// replacement for the brace-prefill trick (see EncodeRequest).
// It is deliberately strict about NOT wrapping the output in
// markdown fences or surrounding prose so the decoded content parses as
// JSON without any post-processing on the decode side.
const anthropicJSONObjectInstruction = "You must respond with a single valid JSON object only. " +
	"Output the raw JSON directly — do not wrap it in markdown code fences and do not add any text, " +
	"explanation, or commentary before or after the JSON."

// appendSystemInstruction appends instruction to the Anthropic top-level
// `system` value, preserving whatever shape it already holds. The codec
// produces three shapes for `system`: absent (nil — no system turns),
// a plain string (exactly one system turn), or a []map[string]any of
// text blocks (multiple system turns). The instruction is added in the
// matching shape so the resulting body stays valid Anthropic Messages
// wire format. An unexpected type falls back to the bare instruction.
func appendSystemInstruction(existing any, instruction string) any {
	switch v := existing.(type) {
	case nil:
		return instruction
	case string:
		if v == "" {
			return instruction
		}
		return v + "\n\n" + instruction
	case []map[string]any:
		return append(v, map[string]any{"type": "text", "text": instruction})
	default:
		return instruction
	}
}

// anthropicSupportedRequestFields lists the canonical OpenAI top-level
// keys anthropic.codec actively maps onto an Anthropic Messages
// request. Anything else surfaces a one-shot WARN per process via
// canonicalext.ScanUnsupported so operators see drift between the hub
// subset and the codec without scanning each request body manually.
var anthropicSupportedRequestFields = map[string]struct{}{
	"model":                 {},
	"messages":              {},
	"max_tokens":            {},
	"max_completion_tokens": {},
	"temperature":           {},
	"top_p":                 {},
	"top_k":                 {},
	"stop":                  {},
	"stream":                {},
	"stream_options":        {},
	"tools":                 {},
	"tool_choice":           {},
	"parallel_tool_calls":   {},
	"metadata":              {},
	"response_format":       {},
}

// splitMessages separates OpenAI `messages` into Anthropic's
// (system prompt, messages) pair. System turns are concatenated as
// raw strings because that is what every client produces today; if an
// OpenAI request ever carries structured system content we fall back
// to stringifying the JSON segment.
func splitMessages(messages gjson.Result) ([]string, []map[string]any, error) {
	var system []string
	var out []map[string]any
	if !messages.IsArray() {
		return system, out, nil
	}
	var splitErr error
	messages.ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		content := msg.Get("content")
		if role == "system" {
			if s := stringifyContent(content); s != "" {
				system = append(system, s)
			}
			return true
		}
		if role == "tool" {
			tid := msg.Get("tool_call_id").String()
			body := stringifyContent(content)
			out = append(out, map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type":        "tool_result",
					"tool_use_id": tid,
					"content":     body,
				}},
			})
			return true
		}
		if role == "" {
			role = "user"
		}
		entry := map[string]any{"role": role}
		if role == "assistant" && msg.Get("tool_calls").Exists() {
			var parts []map[string]any
			// Thinking blocks passed back on an assistant turn must lead
			// the content array and carry their signatures. The only accepted
			// source is the per-block nexus_thinking exact-replay carrier;
			// ordinary reasoning text is never promoted to native thinking.
			parts = append(parts, reconstructThinkingBlocks(msg)...)
			if text := stringifyContent(content); text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
			msg.Get("tool_calls").ForEach(func(_, call gjson.Result) bool {
				fn := call.Get("function")
				args := fn.Get("arguments").String()
				if args == "" {
					args = "{}"
				}
				var inputObj map[string]any
				if err := json.Unmarshal([]byte(args), &inputObj); err != nil || inputObj == nil {
					inputObj = map[string]any{}
				}
				parts = append(parts, map[string]any{
					"type":  "tool_use",
					"id":    call.Get("id").String(),
					"name":  fn.Get("name").String(),
					"input": inputObj,
				})
				return true
			})
			if len(parts) == 0 {
				parts = []map[string]any{{"type": "text", "text": ""}}
			}
			entry["content"] = parts
			out = append(out, entry)
			return true
		}
		// A plain assistant turn (no tool_calls) that carried thinking
		// history gets the same leading signed thinking blocks.
		var thinkPrefix []map[string]any
		if role == "assistant" {
			thinkPrefix = reconstructThinkingBlocks(msg)
		}
		text := stringifyContent(content)
		if text != "" && !content.IsArray() {
			entry["content"] = append(thinkPrefix, map[string]any{"type": "text", "text": text})
			out = append(out, entry)
			return true
		}
		if content.IsArray() {
			parts, err := openAIPartsToAnthropicContent(content)
			if err != nil {
				splitErr = err
				return false
			}
			if len(thinkPrefix) > 0 {
				parts = append(thinkPrefix, parts...)
			}
			if len(parts) > 0 {
				entry["content"] = parts
			} else {
				entry["content"] = []map[string]any{{"type": "text", "text": ""}}
			}
			out = append(out, entry)
			return true
		}
		if len(thinkPrefix) > 0 {
			entry["content"] = thinkPrefix
			out = append(out, entry)
			return true
		}
		entry["content"] = []map[string]any{{"type": "text", "text": ""}}
		out = append(out, entry)
		return true
	})
	return system, out, splitErr
}

func stringifyContent(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		var buf string
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "text" {
				if buf != "" {
					buf += "\n"
				}
				buf += part.Get("text").String()
			}
			return true
		})
		return buf
	}
	return ""
}

// DecodeResponse converts a non-streaming Anthropic response to
// OpenAI chat-completions shape and extracts token usage.
//
// Three steps:
//  1. AnthropicMessagesNormalizer.Normalize parses the raw body into a
//     NormalizedPayload — the same parse the audit / compliance / agent
//     pipelines use.
//  2. normcodecs.ProjectToOpenAIChatCompletion projects NormalizedPayload +
//     wire metadata (id, model, finish-reason) into OpenAI chat-completion JSON.
//  3. Anthropic-specific extras are layered in:
//     a. usage.prompt_tokens_details.cache_creation_tokens — the write-side
//     cache counter (no OpenAI standard equivalent).
//     b. nexus.ext.anthropic.cache_creation_input_tokens — same value under
//     the canonical-extension namespace so the encode path can round-trip
//     it back to Anthropic targets (provider-adapter-architecture.md §3a Rule 4).
func (Codec) DecodeResponse(endpoint typology.WireShape, nativeBody []byte, _ string, _ provcore.DecodeContext) (provcore.DecodeResult, error) {
	if endpoint != typology.WireShapeAnthropicMessages {
		// Models endpoint and anything else is passthrough.
		return provcore.DecodeResult{CanonicalBody: nativeBody}, nil
	}
	if len(nativeBody) == 0 {
		return provcore.DecodeResult{CanonicalBody: nativeBody}, nil
	}
	root := gjson.ParseBytes(nativeBody)

	// Step 1: Tier-1 normalize. Same parser the audit pipeline uses.
	// On parse failure fall through to a zero NormalizedPayload — the
	// projector tolerates an empty assistant message.
	n := normcodecs.NewAnthropicMessagesNormalizer()
	payload, normErr := n.Normalize(context.Background(), nativeBody, normcore.Meta{
		AdapterType: "anthropic",
		Direction:   normcore.DirectionResponse,
	})
	if normErr != nil {
		// Defensive: an unparseable body still flows to the canonical
		// projector with an empty assistant message so cross-format
		// callers always get a well-formed shape. provcore.ExtractUsage
		// below independently returns zero-Usage on the same error.
		payload = normcore.NormalizedPayload{Kind: normcore.KindAIChat}
	}

	// Step 2: Usage via shared normalizer.
	usage := provcore.ExtractUsage(nativeBody, provcore.FormatAnthropic)

	// Step 3: project to OpenAI shape via shared helper.
	created := root.Get("created_at").Int()
	if created == 0 {
		// Anthropic Messages API does not return a created_at.
		created = time.Now().Unix()
	}
	canon, err := normcodecs.ProjectToOpenAIChatCompletion(payload, normcodecs.ProjectionWireMetadata{
		ID:           root.Get("id").String(),
		Model:        root.Get("model").String(),
		Created:      created,
		FinishReason: MapStopReason(root.Get("stop_reason").String()),
		Usage:        UsageToNormalize(usage),
	})
	if err != nil {
		return provcore.DecodeResult{CanonicalBody: nativeBody, Usage: usage}, err
	}

	// Step 4: Preserve provider-owned thinking blocks as an ordered exact-
	// replay carrier. The shared normalized payload intentionally keeps only
	// universal reasoning text, so recover signed and redacted native blocks
	// from the original response here.
	var nexusThinking []map[string]any
	root.Get("content").ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").String() {
		case "thinking":
			if sig := block.Get("signature").String(); sig != "" {
				nexusThinking = append(nexusThinking, map[string]any{
					"thinking": block.Get("thinking").String(), "signature": sig,
				})
			}
		case "redacted_thinking":
			if data := block.Get("data").String(); data != "" {
				nexusThinking = append(nexusThinking, map[string]any{"redacted_data": data})
			}
		}
		return true
	})
	if len(nexusThinking) > 0 {
		canon, err = sjson.SetBytes(canon, "choices.0.message.nexus_thinking", nexusThinking)
		if err != nil {
			return provcore.DecodeResult{CanonicalBody: nativeBody, Usage: usage}, err
		}
	}

	// Step 5: Anthropic-specific wire extras.
	//
	// prompt_tokens_details.cache_creation_tokens — Anthropic reports
	// cache_creation_input_tokens (write-side, billed at 1.25x premium)
	// separately from cache_read_input_tokens. OpenAI only defines the read
	// side; we surface the write count in the same details object, beside
	// cached_tokens.
	//
	// This is the ONLY place the number is written. It used to be written
	// twice — here, and again into nexus.ext.anthropic.cache_creation_input_tokens
	// for the Anthropic-wire egress converter to read back. That second copy was
	// a carrier in a namespace the gateway treats as internal on the request
	// side, and nothing removed it on the response side, so an OpenAI-wire caller
	// received `"nexus":{"ext":{"anthropic":{…}}}` in its response body. The
	// converter reads this canonical field instead; one number has one home.
	if u := root.Get("usage"); u.Exists() {
		if v := u.Get("cache_creation_input_tokens"); v.Exists() && v.Int() > 0 {
			canon, err = sjson.SetBytes(canon, "usage.prompt_tokens_details.cache_creation_tokens", v.Int())
			if err != nil {
				return provcore.DecodeResult{CanonicalBody: nativeBody, Usage: usage}, err
			}
		}
	}
	return provcore.DecodeResult{CanonicalBody: canon, Usage: usage}, nil
}

// usageToNormalize converts a provcore.Usage (which is type-aliased to
// normcore.Usage) to the *normcore.Usage pointer the projector
// expects. Returns nil for a zero Usage so the projector omits the
// "usage" key entirely.
func UsageToNormalize(u provcore.Usage) *normcore.Usage {
	if u.PromptTokens == nil && u.CompletionTokens == nil && u.TotalTokens == nil &&
		u.CacheReadTokens == nil && u.CacheCreationTokens == nil && u.ReasoningTokens == nil {
		return nil
	}
	v := u
	return &v
}

// mapStopReason translates Anthropic stop_reason to the canonical OpenAI
// finish_reason enum. Unknown values pass through unchanged so operators
// can spot drift in upstream APIs without losing the raw signal.
//
// The documented Anthropic vocabulary is end_turn / max_tokens /
// stop_sequence / tool_use / pause_turn / refusal /
// model_context_window_exceeded. end_turn and stop_sequence both collapse to
// "stop": OpenAI's stop genuinely covers both, so that is a lossy projection
// onto a smaller vocabulary rather than a wrong value.
//
// pause_turn deliberately passes through. It means the turn was suspended
// and may be resumed by resubmitting the response as-is — no OpenAI
// finish_reason carries that instruction, and folding it into "stop" would
// tell the caller the answer is finished when it is resumable.
func MapStopReason(r string) string {
	switch r {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return r
}

// anthropicFallbackMaxOutput is the output cap used ONLY when the catalog
// leaves maxOutputTokens unset (the column is nullable) and the caller sent
// no cap of its own. Anthropic rejects the request outright without the
// field, so some number must go on the wire; 8192 is the conservative
// across-Claude floor that every Claude model accepts. It is deliberately
// not a per-model table — the catalog owns per-model ceilings, and a second
// table here would drift from the value /v1/models advertises.
const anthropicFallbackMaxOutput = 8192

// clampMaxTokens bounds a caller-supplied output cap to the model's
// advertised ceiling. Anthropic hard-rejects an over-ceiling request
// (400 "max_tokens: N > M, which is the maximum allowed number of output
// tokens for <model>"), so forwarding the caller's number verbatim turns a
// satisfiable request into a failure. Clamping instead yields the most the
// model can actually produce, and the rewrite makes the coercion visible via
// the x-nexus-coerced response header rather than silently changing intent.
//
// limit <= 0 means the catalog has no ceiling for this model; the caller's
// value is then forwarded untouched — inventing a bound we cannot justify
// would truncate responses the model may well support.
func clampMaxTokens(requested int64, limit int, rewrites *[]string) int64 {
	if limit <= 0 || requested <= int64(limit) {
		return requested
	}
	*rewrites = append(*rewrites, fmt.Sprintf("max_tokens→%d_model_max", limit))
	return int64(limit)
}

// Anthropic's extended-thinking constraints, probed against
// api.anthropic.com on 2026-08-06 with claude-haiku-4-5:
//
//	max_tokens 2048, budget 1024  -> 200
//	max_tokens 1024, budget 1024  -> 400 "`max_tokens` must be greater than `thinking.budget_tokens`"
//	max_tokens 1024, budget 2048  -> 400 same
//	max_tokens 1024, budget  512  -> 400 "budget_tokens: Input should be greater than or equal to 1024"
//
// So max_tokens is the TOTAL when thinking is on — there would be no reason
// to require it exceed the thinking budget otherwise — and the budget has a
// hard floor of its own.
const (
	anthropicMinThinkingBudget = 1024
)

// reconcileThinkingBudget keeps max_tokens and thinking.budget_tokens
// consistent before the request leaves.
//
// The thinking block was forwarded verbatim on the stated grounds that the
// gateway does not validate it and any rejection is the caller's to see. That
// holds only while the numbers are the caller's. They are not: max_tokens
// immediately above is clamped by us to the model's ceiling, or filled by us
// when the caller omitted it. A caller who sent a consistent pair can have it
// made inconsistent by our own clamp and then receive a 400 describing a
// request they never sent.
//
// Resolution order matters. Lowering the budget to fit under the cap is the
// minimal change that preserves what the caller asked for — thinking, within
// the cap they set. Raising max_tokens instead would push past the model
// ceiling the clamp exists to respect, trading this 400 for a different one.
//
// When the cap cannot house the 1024 floor at all, thinking is impossible
// within it. That is refused here rather than forwarded, because our error can
// name the cap and the floor together while Anthropic's can only report
// whichever bound it checked first.
func reconcileThinkingBudget(out map[string]any, thinking map[string]any, rewrites *[]string) error {
	if t, _ := thinking["type"].(string); t == "disabled" {
		return nil
	}
	budget, ok := numericField(thinking["budget_tokens"])
	if !ok {
		// No budget to reconcile. An unparseable one stays the caller's
		// problem, as the passthrough comment says.
		return nil
	}
	maxTokens, ok := numericField(out["max_tokens"])
	if !ok || maxTokens <= 0 {
		return nil
	}
	fitted, err := fitThinkingBudget(budget, maxTokens)
	if err != nil {
		return err
	}
	if fitted == budget {
		return nil
	}
	thinking["budget_tokens"] = fitted
	*rewrites = append(*rewrites, fmt.Sprintf("thinking.budget_tokens→%d_fits_max_tokens", fitted))
	return nil
}

// fitThinkingBudget is the DECISION both doors ask, so neither can answer it
// differently.
//
// The cross-format door rebuilds the body from the canonical and the native
// door edits the caller's own bytes, but the arithmetic is the same and the
// consequence of disagreeing is the same 400 describing a request nobody sent.
// It was written on one door only, and the native door — which clamps
// max_tokens with the very same policy — stranded the budget above the cap it
// had just lowered.
//
// Returns budget unchanged when it already fits.
func fitThinkingBudget(budget, maxTokens int64) (int64, error) {
	if budget < maxTokens {
		return budget, nil
	}
	fitted := maxTokens - 1
	if fitted < anthropicMinThinkingBudget {
		return 0, errUnsupportedField(fmt.Sprintf(
			"thinking.budget_tokens: extended thinking needs a budget of at least %d and max_tokens strictly above it, "+
				"but max_tokens is %d for this model", anthropicMinThinkingBudget, maxTokens))
	}
	return fitted, nil
}

// thinkingFromCanonicalEffort converts the canonical LEVEL into this wire's
// BUDGET, or nil when the caller expressed nothing.
//
// The conversion invents no per-model data. It uses the only two numbers this
// codec has probe evidence for — the 1024 floor, and the requirement that the
// budget sit strictly below max_tokens — and derives everything else from the
// cap the caller (or our own clamp) already set. A guessed per-model range
// would be an upstream 400 dressed as a feature.
//
// "none" produces nothing: the caller asked NOT to think, and this wire spells
// that by omitting the block rather than by sending a zero budget.
//
// A cap that cannot house the floor is left to reconcileThinkingBudget, which
// refuses with an error naming both numbers — the same answer a caller who
// spoke Anthropic natively would get.
func thinkingFromCanonicalEffort(canonicalBody []byte, out map[string]any) map[string]any {
	effort := gjson.GetBytes(canonicalBody, "reasoning_effort")
	if !effort.Exists() || effort.Type != gjson.String {
		return nil
	}
	maxTokens, ok := numericField(out["max_tokens"])
	if !ok || maxTokens <= anthropicMinThinkingBudget {
		// Nothing fits. Returning nil forwards the request without thinking
		// rather than refusing it: the caller expressed a preference, not a
		// requirement, and a refusal here would take away an answer they can
		// still use. The eligibility filter upstream is what keeps a request
		// that NEEDS reasoning away from a model that cannot.
		return nil
	}
	ceiling := maxTokens - 1

	var budget int64
	switch strings.ToLower(effort.String()) {
	case "none":
		return nil
	case "minimal", "low":
		budget = anthropicMinThinkingBudget
	case "medium":
		budget = anthropicMinThinkingBudget + (ceiling-anthropicMinThinkingBudget)/2
	case "high", "max":
		budget = ceiling
	default:
		// An effort level this wire's vocabulary does not contain. Guessing
		// which end of the range an unknown word means is how a translation
		// starts producing answers nobody asked for.
		return nil
	}
	return map[string]any{"type": "enabled", "budget_tokens": budget}
}

// numericField reads a JSON number that may have decoded as float64 (the
// json.Unmarshal default) or as an integer type.
func numericField(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}
