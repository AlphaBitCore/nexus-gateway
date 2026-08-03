package codec

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
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
	if rf := root.Get("response_format"); rf.Exists() {
		switch rf.Get("type").String() {
		case "json_object":
			// The Anthropic Messages API has no native json_object mode.
			// The widely-used "prefill" trick (append an assistant turn
			// whose content is a bare "{") forces JSON but is silently
			// broken across this gateway: Anthropic completes the object
			// WITHOUT re-emitting the prefilled "{", and neither the
			// non-streaming DecodeResponse nor the SSE stream path can
			// re-prepend it — the SchemaCodec/StreamDecoder interfaces are
			// stateless and never see the originating request, so they
			// cannot know a "{" was prefilled. The caller therefore
			// received content beginning mid-object ("k":1}) that fails
			// JSON.parse 100% of the time.
			//
			// Instead we force JSON via a system instruction. Anthropic
			// emits the complete object (including the opening "{"), so the
			// decode/stream paths pass it through unchanged and the caller
			// gets parseable JSON. The instruction is appended to whatever
			// system content already exists (none / string / text blocks).
			out["system"] = appendSystemInstruction(out["system"], anthropicJSONObjectInstruction)
		case "json_schema":
			return provcore.EncodeResult{}, errUnsupportedField("response_format.json_schema")
		}
	}
	out["messages"] = messages

	// nexus.ext.anthropic.thinking passthrough: clients targeting an
	// Anthropic-protocol upstream (native Anthropic or Bedrock Claude)
	// opt in to extended thinking by placing the Anthropic-native shape
	// under nexus.ext.anthropic.thinking. We forward it verbatim — the
	// gateway does not validate the inner shape; if Anthropic rejects
	// it, the error surfaces to the client unmodified.
	if ext := canonicalext.Get(canonicalBody, "anthropic", "thinking"); ext.Exists() {
		if ext.IsObject() {
			var thinking map[string]any
			if err := json.Unmarshal([]byte(ext.Raw), &thinking); err == nil && len(thinking) > 0 {
				out["thinking"] = thinking
				provdispatch.EmitReasoningPassthrough("anthropic", "injected")
			} else {
				canonicalext.WarnOnce("anthropic", "thinking_unmarshal_failed")
				provdispatch.EmitReasoningPassthrough("anthropic", "skipped_malformed")
			}
		} else {
			canonicalext.WarnOnce("anthropic", "thinking_not_object")
			provdispatch.EmitReasoningPassthrough("anthropic", "skipped_malformed")
		}
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
			// the content array and carry their signatures — Anthropic
			// validates each signature on returned thinking. Per-block
			// carrier is nexus_thinking (set by the Anthropic-ingress
			// converter); a cross-format upstream (DeepSeek/OpenAI) yields
			// one unsigned block from reasoning_content, which Anthropic
			// accepts on request bodies it did not itself sign.
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

func openAIPartsToAnthropicContent(content gjson.Result) ([]map[string]any, error) {
	var parts []map[string]any
	var err error
	content.ForEach(func(_, part gjson.Result) bool {
		if err != nil {
			return false
		}
		switch part.Get("type").String() {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": part.Get("text").String()})
		case "image_url":
			detail := part.Get("image_url.detail").String()
			if detail == "high" {
				err = errUnsupportedField("image_url.detail=high")
				return false
			}
			url := part.Get("image_url.url").String()
			if url == "" {
				err = errUnsupportedField("image_url.url")
				return false
			}
			if strings.HasPrefix(url, "data:") {
				media, b64, ok := ParseDataURL(url)
				if !ok {
					err = errUnsupportedField("image_url.url(data:invalid)")
					return false
				}
				parts = append(parts, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": media,
						"data":       b64,
					},
				})
			} else {
				parts = append(parts, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type": "url",
						"url":  url,
					},
				})
			}
		case "tool_result":
			parts = append(parts, map[string]any{
				"type":        "tool_result",
				"tool_use_id": part.Get("tool_call_id").String(),
				"content":     StringifyOpenAIToolResultContent(part.Get("content")),
			})
		default:
			var m map[string]any
			if uerr := json.Unmarshal([]byte(part.Raw), &m); uerr == nil {
				parts = append(parts, m)
			}
		}
		return true
	})
	return parts, err
}

func StringifyOpenAIToolResultContent(c gjson.Result) string {
	if c.Type == gjson.String {
		return c.String()
	}
	return c.Raw
}

func ParseDataURL(dataURL string) (mediaType, b64 string, ok bool) {
	if !strings.HasPrefix(dataURL, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(dataURL, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 || comma == len(rest)-1 {
		return "", "", false
	}
	meta, payload := rest[:comma], rest[comma+1:]
	if !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	mediaType = strings.TrimSuffix(meta, ";base64")
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return "", "", false
	}
	return mediaType, payload, true
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

	// Step 4: Anthropic-specific wire extras.
	//
	// 4a) prompt_tokens_details.cache_creation_tokens — Anthropic reports
	//     cache_creation_input_tokens (write-side, billed at 1.25x premium)
	//     separately from cache_read_input_tokens. OpenAI only defines the
	//     read side; we surface the write count in the same details object.
	// 4b) nexus.ext.anthropic.cache_creation_input_tokens — same value under
	//     the canonical-extension namespace so the encode path can round-trip
	//     it back to Anthropic targets (provider-adapter-architecture.md §3a Rule 4).
	if u := root.Get("usage"); u.Exists() {
		if v := u.Get("cache_creation_input_tokens"); v.Exists() && v.Int() > 0 {
			canon, err = sjson.SetBytes(canon, "usage.prompt_tokens_details.cache_creation_tokens", v.Int())
			if err != nil {
				return provcore.DecodeResult{CanonicalBody: nativeBody, Usage: usage}, err
			}
		}
		if v := u.Get("cache_creation_input_tokens"); v.Exists() && v.Int() > 0 {
			canon, err = canonicalext.Set(canon, "anthropic", "cache_creation_input_tokens", v.Int())
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
func MapStopReason(r string) string {
	switch r {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
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
