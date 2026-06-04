package codec

import (
	"context"
	"encoding/json"
	"fmt"
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

// errUnsupportedField surfaces hard-rule errors as a structured
// *provcore.ProviderError so the client receives a 400 with a stable
// type string instead of a free-form fmt.Errorf message.
func errUnsupportedField(field string) error {
	return &provcore.ProviderError{
		Status:  http.StatusBadRequest,
		Code:    provcore.CodeInvalidRequest,
		Type:    "nexus_field_unsupported",
		Message: "nexus: field " + field + " unsupported on this route",
	}
}

// Codec translates OpenAI `/v1/chat/completions` ↔ Gemini
// `generateContent` bodies. Mapping focuses on the common subset:
// messages → contents with roles "user"/"model", system message →
// systemInstruction, temperature/top_p/max_tokens → generationConfig.
type Codec struct{}

// NewCodec returns a Codec as a provcore.SchemaCodec.
func NewCodec() provcore.SchemaCodec { return Codec{} }

// EncodeRequest canonical OpenAI → Gemini. The same codec serves both
// Gemini (Google AI Studio) and Vertex AI — the bodies are wire-identical,
// so the Vertex AdapterSpec re-uses this codec and the shape gate accepts
// both WireShapeGeminiGenerateContent and WireShapeVertexGenerateContent
// (likewise for the embeddings shapes).
func (Codec) EncodeRequest(endpoint typology.WireShape, canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	if endpoint == typology.WireShapeGeminiEmbedContent || endpoint == typology.WireShapeVertexEmbedContent {
		return encodeGeminiEmbeddingRequest(canonicalBody, target)
	}
	if endpoint != typology.WireShapeGeminiGenerateContent && endpoint != typology.WireShapeVertexGenerateContent {
		return provcore.EncodeResult{}, fmt.Errorf("gemini: unsupported endpoint %q for codec", endpoint)
	}
	_ = target // target is unused on the chat path (authentication is on the transport)
	if len(canonicalBody) == 0 {
		return provcore.EncodeResult{}, fmt.Errorf("gemini: empty canonical body")
	}
	root := gjson.ParseBytes(canonicalBody)

	genCfg := map[string]any{}
	if v := root.Get("temperature"); v.Exists() {
		genCfg["temperature"] = v.Float()
	}
	if v := root.Get("top_p"); v.Exists() {
		genCfg["topP"] = v.Float()
	}
	if v := root.Get("top_k"); v.Exists() {
		genCfg["topK"] = v.Int()
	}
	// max_completion_tokens overrides max_tokens when both are present
	// (matches OpenAI reasoning-model semantics where max_tokens is
	// silently ignored).
	if v := root.Get("max_completion_tokens"); v.Exists() {
		genCfg["maxOutputTokens"] = v.Int()
	} else if v := root.Get("max_tokens"); v.Exists() {
		genCfg["maxOutputTokens"] = v.Int()
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
				genCfg["stopSequences"] = list
			}
		case stop.Type == gjson.String:
			genCfg["stopSequences"] = []string{stop.String()}
		}
	}

	out := map[string]any{}
	if len(genCfg) > 0 {
		out["generationConfig"] = genCfg
	}

	system, contents, err := splitMessages(root.Get("messages"))
	if err != nil {
		return provcore.EncodeResult{}, err
	}
	if system != "" {
		out["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": system}},
		}
	}
	if len(contents) == 0 {
		return provcore.EncodeResult{}, fmt.Errorf("gemini: no messages")
	}
	out["contents"] = contents

	if tools := root.Get("tools"); tools.IsArray() && len(tools.Array()) > 0 {
		var decls []map[string]any
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
			params := fn.Get("parameters")
			var paramsObj any
			if params.Exists() && params.Raw != "" {
				_ = json.Unmarshal([]byte(params.Raw), &paramsObj)
			}
			if paramsObj == nil {
				paramsObj = map[string]any{"type": "object"}
			}
			decls = append(decls, map[string]any{
				"name":        name,
				"description": desc,
				"parameters":  paramsObj,
			})
			return true
		})
		if len(decls) > 0 {
			out["tools"] = []map[string]any{{"functionDeclarations": decls}}
		}
	}
	if tc := root.Get("tool_choice"); tc.Exists() {
		mode := "AUTO"
		var allowed []string
		switch tc.Type {
		case gjson.String:
			switch tc.String() {
			case "none":
				mode = "NONE"
			case "required":
				mode = "ANY"
			case "auto":
				mode = "AUTO"
			}
		case gjson.JSON:
			if tc.Get("type").String() == "function" {
				mode = "ANY"
				name := tc.Get("function.name").String()
				if name != "" {
					allowed = []string{name}
				}
			}
		}
		fcc := map[string]any{"mode": mode}
		if len(allowed) > 0 {
			fcc["allowedFunctionNames"] = allowed
		}
		out["toolConfig"] = map[string]any{"functionCallingConfig": fcc}
	}
	if rf := root.Get("response_format"); rf.Exists() {
		switch rf.Get("type").String() {
		case "json_object":
			gen, _ := out["generationConfig"].(map[string]any)
			if gen == nil {
				gen = map[string]any{}
			}
			gen["responseMimeType"] = "application/json"
			out["generationConfig"] = gen
		case "json_schema":
			gen, _ := out["generationConfig"].(map[string]any)
			if gen == nil {
				gen = map[string]any{}
			}
			gen["responseMimeType"] = "application/json"
			if js := rf.Get("json_schema"); js.Exists() {
				var schema any
				if err := json.Unmarshal([]byte(js.Raw), &schema); err == nil {
					gen["responseSchema"] = schema
				}
			}
			out["generationConfig"] = gen
		}
	}

	// nexus.ext.gemini.thinking_config passthrough: clients targeting a
	// Gemini upstream opt in to thinking summary by placing the Gemini-native
	// shape under nexus.ext.gemini.thinking_config. We forward it verbatim into
	// generationConfig.thinkingConfig, merging with any existing generationConfig
	// keys (temperature / responseMimeType / etc.) already populated. Gemini-side
	// validation of inner subkeys is upstream's job.
	if ext := canonicalext.Get(canonicalBody, "gemini", "thinking_config"); ext.Exists() {
		if ext.IsObject() {
			var thinkingCfg map[string]any
			if err := json.Unmarshal([]byte(ext.Raw), &thinkingCfg); err == nil && len(thinkingCfg) > 0 {
				gen, _ := out["generationConfig"].(map[string]any)
				if gen == nil {
					gen = map[string]any{}
				}
				gen["thinkingConfig"] = thinkingCfg
				out["generationConfig"] = gen
				provdispatch.EmitReasoningPassthrough("gemini", "injected")
			} else {
				canonicalext.WarnOnce("gemini", "thinking_config_unmarshal_failed")
				provdispatch.EmitReasoningPassthrough("gemini", "skipped_malformed")
			}
		} else {
			canonicalext.WarnOnce("gemini", "thinking_config_not_object")
			provdispatch.EmitReasoningPassthrough("gemini", "skipped_malformed")
		}
	}

	canonicalext.ScanUnsupported("gemini", canonicalBody, geminiSupportedRequestFields)

	body, err := json.Marshal(out)
	if err != nil {
		return provcore.EncodeResult{}, err
	}
	return provcore.EncodeResult{Body: body, ContentType: "application/json"}, nil
}

// geminiSupportedRequestFields lists the canonical OpenAI top-level keys
// gemini.codec maps onto a generateContent request. Drift surfaces
// once per (provider, field) tuple via canonicalext.ScanUnsupported.
var geminiSupportedRequestFields = map[string]struct{}{
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
	"response_format":       {},
}

func splitMessages(messages gjson.Result) (string, []map[string]any, error) {
	var system string
	var out []map[string]any
	var splitErr error
	if !messages.IsArray() {
		return system, out, nil
	}
	idToName := map[string]string{}
	messages.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "assistant" || !msg.Get("tool_calls").Exists() {
			return true
		}
		msg.Get("tool_calls").ForEach(func(_, call gjson.Result) bool {
			id := call.Get("id").String()
			name := call.Get("function.name").String()
			if id != "" && name != "" {
				idToName[id] = name
			}
			return true
		})
		return true
	})

	messages.ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		if role == "system" {
			text := StringifyContent(msg.Get("content"))
			if system != "" {
				system += "\n"
			}
			system += text
			return true
		}
		if role == "tool" {
			tid := msg.Get("tool_call_id").String()
			fnName := idToName[tid]
			if fnName == "" {
				fnName = tid
			}
			if fnName == "" {
				fnName = "unknown"
			}
			// Gemini's functionResponse.response is documented as an
			// object (struct), not a free-form value. Canonical OpenAI
			// tool messages carry content as a string, so we probe:
			//   1. JSON object literal → forward as-is (round-trips a
			//      Gemini ingress request without restructuring).
			//   2. anything else (plain string, JSON array, scalar) →
			//      wrap as {"result": <value>} so the upstream schema
			//      is satisfied.
			raw := msg.Get("content")
			var resp any = map[string]any{"result": StringifyContent(raw)}
			switch {
			case raw.IsObject():
				var v map[string]any
				if err := json.Unmarshal([]byte(raw.Raw), &v); err == nil {
					resp = v
				}
			case raw.Type == gjson.String:
				if s := raw.String(); s != "" {
					var v map[string]any
					if err := json.Unmarshal([]byte(s), &v); err == nil {
						resp = v
					} else {
						resp = map[string]any{"result": s}
					}
				}
			case raw.IsArray():
				var v any
				if err := json.Unmarshal([]byte(raw.Raw), &v); err == nil {
					resp = map[string]any{"result": v}
				}
			}
			fr := map[string]any{
				"name":     fnName,
				"response": resp,
			}
			// Gemini 3 multi-tool turns require the call id to be
			// echoed back on functionResponse so the model can match
			// the response to its earlier functionCall. Older models
			// do not emit/accept id; only forward when canonical
			// supplied one (we never inject a synthesized id here).
			if tid != "" {
				fr["id"] = tid
			}
			out = append(out, map[string]any{
				"role": "user",
				"parts": []map[string]any{{
					"functionResponse": fr,
				}},
			})
			return true
		}
		geminiRole := role
		if role == "assistant" {
			geminiRole = "model"
		}
		if geminiRole == "" {
			geminiRole = "user"
		}
		parts, err := openAIMessageToGeminiParts(msg)
		if err != nil {
			splitErr = err
			return false
		}
		out = append(out, map[string]any{
			"role":  geminiRole,
			"parts": parts,
		})
		return true
	})
	return system, out, splitErr
}

func openAIMessageToGeminiParts(msg gjson.Result) ([]map[string]any, error) {
	var parts []map[string]any
	content := msg.Get("content")
	if msg.Get("tool_calls").Exists() && msg.Get("tool_calls").IsArray() {
		msg.Get("tool_calls").ForEach(func(_, call gjson.Result) bool {
			fn := call.Get("function")
			args := fn.Get("arguments").String()
			if args == "" {
				args = "{}"
			}
			var argsObj any
			if err := json.Unmarshal([]byte(args), &argsObj); err != nil {
				argsObj = map[string]any{}
			}
			fc := map[string]any{
				"name": fn.Get("name").String(),
				"args": argsObj,
			}
			// Only Gemini 3+ accepts functionCall.id on the request body.
			// Older models (1.5 / 2.x) reject the field as unknown, so
			// forward it only when canonical actually supplied one. The
			// reverse direction (DecodeResponse / stream) still
			// synthesizes an id when Gemini 3+ omits it on response, so
			// OpenAI clients always see a stable tool_call_id.
			// Doc: https://ai.google.dev/gemini-api/docs/function-calling
			if id := call.Get("id").String(); id != "" {
				fc["id"] = id
			}
			parts = append(parts, map[string]any{
				"functionCall": fc,
			})
			return true
		})
	}
	if content.Type == gjson.String {
		if s := content.String(); s != "" {
			parts = append(parts, map[string]any{"text": s})
		}
		return parts, nil
	}
	var partsErr error
	if content.IsArray() {
		content.ForEach(func(_, part gjson.Result) bool {
			if partsErr != nil {
				return false
			}
			switch part.Get("type").String() {
			case "text":
				parts = append(parts, map[string]any{"text": part.Get("text").String()})
			case "image_url":
				if part.Get("image_url.detail").String() == "high" {
					partsErr = errUnsupportedField("image_url.detail=high")
					return false
				}
				url := part.Get("image_url.url").String()
				if url == "" {
					partsErr = errUnsupportedField("image_url.url")
					return false
				}
				if strings.HasPrefix(url, "data:") {
					media, b64, ok := ParseDataURL(url)
					if !ok {
						partsErr = errUnsupportedField("image_url.url(data:invalid)")
						return false
					}
					parts = append(parts, map[string]any{
						"inlineData": map[string]any{
							"mimeType": media,
							"data":     b64,
						},
					})
				} else {
					parts = append(parts, map[string]any{
						"fileData": map[string]any{
							"mimeType": GuessMimeFromURL(url),
							"fileUri":  url,
						},
					})
				}
			}
			return true
		})
	}
	if partsErr != nil {
		return nil, partsErr
	}
	if len(parts) == 0 {
		parts = []map[string]any{{"text": ""}}
	}
	return parts, nil
}

// ParseDataURL extracts the media type and base64 payload from a data: URL.
// Returns ok=false on shapes the codec cannot turn into a Gemini inlineData
// part (missing comma, missing ;base64, malformed payload). Exported for tests.
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
	return mediaType, payload, payload != ""
}

// GuessMimeFromURL maps a remote image URL to a best-effort Gemini mimeType
// by extension (after stripping any query string). Falls back to image/jpeg
// when the extension is missing or unrecognised so the upstream call still
// succeeds; operators can override via the Extras pipeline if a specific
// mimeType is required. Exported for tests.
func GuessMimeFromURL(u string) string {
	lower := strings.ToLower(u)
	if i := strings.Index(lower, "?"); i >= 0 {
		lower = lower[:i]
	}
	if i := strings.Index(lower, "#"); i >= 0 {
		lower = lower[:i]
	}
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".heic"):
		return "image/heic"
	case strings.HasSuffix(lower, ".heif"):
		return "image/heif"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	default:
		return "image/jpeg"
	}
}

func StringifyContent(content gjson.Result) string {
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

// DecodeResponse converts a Gemini generateContent response to canonical
// OpenAI chat-completion shape. Delegates the block-walk to
// GeminiGenerateNormalizer + ProjectToOpenAIChatCompletion — the same
// parser the audit / compliance / agent pipeline uses. The codec retains
// Gemini-specific wire-metadata stamping:
//   - id ← responseId, model ← modelVersion (Gemini-specific field names)
//   - finish_reason mapping via MapFinishReason (Gemini vocabulary)
//   - usageMetadata extras: cachedContentTokenCount surfaces as
//     prompt_tokens_details.cached_tokens; thoughtsTokenCount as
//     completion_tokens_details.reasoning_tokens.
//
// Multi-candidate (candidates[] with candidateCount>1) is preserved:
// the Tier-1 normalizer emits one assistant message per candidate, and
// the shared projector turns each assistant message into a choices[]
// entry with its own finish_reason.
func (Codec) DecodeResponse(endpoint typology.WireShape, nativeBody []byte, _ string) (provcore.DecodeResult, error) {
	if endpoint == typology.WireShapeGeminiEmbedContent || endpoint == typology.WireShapeVertexEmbedContent {
		return decodeGeminiEmbeddingResponse(nativeBody)
	}
	if endpoint != typology.WireShapeGeminiGenerateContent && endpoint != typology.WireShapeVertexGenerateContent {
		return provcore.DecodeResult{CanonicalBody: nativeBody}, nil
	}
	if len(nativeBody) == 0 {
		return provcore.DecodeResult{CanonicalBody: nativeBody}, nil
	}
	root := gjson.ParseBytes(nativeBody)

	// Step 1: Tier-1 normalize.
	n := normcodecs.NewGeminiGenerateNormalizer()
	payload, normErr := n.Normalize(context.Background(), nativeBody, normcore.Meta{
		AdapterType: "gemini",
		Direction:   normcore.DirectionResponse,
	})
	if normErr != nil {
		// Defensive: malformed body → projector handles empty payload.
		payload = normcore.NormalizedPayload{Kind: normcore.KindAIChat}
	}
	// Map Gemini per-candidate finish reasons to OpenAI vocabulary
	// before projection so each choices[].finish_reason is correct
	// without an extra post-process pass.
	for i := range payload.Messages {
		payload.Messages[i].FinishReason = MapFinishReason(payload.Messages[i].FinishReason)
	}

	// Step 2: Usage via shared normalizer.
	usage := provcore.ExtractUsage(nativeBody, provcore.FormatGemini)

	// Step 3: project to OpenAI shape.
	canon, err := normcodecs.ProjectToOpenAIChatCompletion(payload, normcodecs.ProjectionWireMetadata{
		ID:      root.Get("responseId").String(),
		Model:   root.Get("modelVersion").String(),
		Created: time.Now().Unix(),
		// FinishReason left empty so the projector picks each
		// candidate's per-message reason (the per-message values we
		// mapped above). The first-choice-meta-wins behaviour does
		// NOT apply here because Gemini multi-candidate responses
		// genuinely want per-candidate reasons.
		Usage: UsageToNormalize(usage),
	})
	if err != nil {
		return provcore.DecodeResult{CanonicalBody: nativeBody, Usage: usage}, err
	}

	// Step 4: Gemini-specific extras the generic projector doesn't know about.
	// cachedContentTokenCount feeds prompt_tokens_details.cached_tokens;
	// thoughtsTokenCount feeds completion_tokens_details.reasoning_tokens.
	// The projector already emits these when Usage carries them; this step
	// stamps them only when Usage was missing those fields (defensive against
	// future normalizer regressions).
	if meta := root.Get("usageMetadata"); meta.Exists() {
		if v := meta.Get("cachedContentTokenCount"); v.Exists() && v.Int() > 0 &&
			!gjson.GetBytes(canon, "usage.prompt_tokens_details.cached_tokens").Exists() {
			canon, err = sjson.SetBytes(canon, "usage.prompt_tokens_details.cached_tokens", v.Int())
			if err != nil {
				return provcore.DecodeResult{CanonicalBody: nativeBody, Usage: usage}, err
			}
		}
		if v := meta.Get("thoughtsTokenCount"); v.Exists() && v.Int() > 0 &&
			!gjson.GetBytes(canon, "usage.completion_tokens_details.reasoning_tokens").Exists() {
			canon, err = sjson.SetBytes(canon, "usage.completion_tokens_details.reasoning_tokens", v.Int())
			if err != nil {
				return provcore.DecodeResult{CanonicalBody: nativeBody, Usage: usage}, err
			}
		}
	}
	return provcore.DecodeResult{CanonicalBody: canon, Usage: usage}, nil
}

// UsageToNormalize converts a provcore.Usage to the *normcore.Usage
// pointer the projector expects. Returns nil for an empty Usage so the
// projector omits the "usage" key entirely. Exported for tests.
func UsageToNormalize(u provcore.Usage) *normcore.Usage {
	if u.PromptTokens == nil && u.CompletionTokens == nil && u.TotalTokens == nil &&
		u.CacheReadTokens == nil && u.CacheCreationTokens == nil && u.ReasoningTokens == nil {
		return nil
	}
	v := u
	return &v
}

// MapFinishReason translates Gemini's documented FinishReason enum into
// the canonical OpenAI finish_reason set. Newer Gemini values
// (MODEL_ARMOR for safety-classifier blocks, UNEXPECTED_TOOL_CALL for
// model-side tool-call validation failures) are folded into the closest
// canonical bucket. Unknown values pass through so operators can spot
// upstream API changes via raw signal rather than silent loss.
// Doc: https://ai.google.dev/api/generate-content#FinishReason
func MapFinishReason(r string) string {
	switch r {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "LANGUAGE", "PROHIBITED_CONTENT",
		"SPII", "BLOCKLIST", "IMAGE_SAFETY", "MODEL_ARMOR":
		return "content_filter"
	case "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL":
		return "tool_calls"
	case "OTHER", "":
		return "stop"
	}
	return r
}
