package codec

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provdispatch "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/dispatch"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	normcodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Surfaces hard-rule errors with a stable type string rather than a free-form
// fmt.Errorf message.
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
	if endpoint == typology.WireShapeGeminiImagesGenerateContent {
		return encodeGeminiImagesRequest(canonicalBody, target)
	}
	if endpoint != typology.WireShapeGeminiGenerateContent && endpoint != typology.WireShapeVertexGenerateContent {
		return provcore.EncodeResult{}, fmt.Errorf("gemini: unsupported endpoint %q for codec", endpoint)
	}
	_ = target // target is unused on the chat path (authentication is on the transport)
	if len(canonicalBody) == 0 {
		return provcore.EncodeResult{}, fmt.Errorf("gemini: empty canonical body")
	}
	root := gjson.ParseBytes(canonicalBody)

	// rewrites collects the in-place coercions this encode applied (degraded
	// tool-schema references) for the x-nexus-coerced report.
	var rewrites []string

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
		var schemaErr error
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
				// Prepared once and shared read-only with concurrent
				// encoders: a declaration is fixed for the life of a
				// conversation and re-arrives every turn.
				//
				// Tool parameters take the LENIENT reference mode — an
				// un-shipped $ref degrades to an open object (rationale and
				// the observed prod 400 live on inlineSchemaRefs).
				// responseSchema below stays strict:
				// there the schema is the caller's output contract.
				prepared, err := prepareGeminiSchema([]byte(params.Raw), true)
				switch {
				case isSchemaRefFailure(err):
					// An unfoldable reference leaves the argument an empty
					// schema and the function silently loses it.
					schemaErr = fmt.Errorf("tool %q parameters: %w", name, err)
					return false
				case err == nil && prepared.object:
					paramsObj = prepared.encoded
					for _, ref := range prepared.droppedRefs {
						// Caller-controlled: cap on a rune boundary so the
						// x-nexus-coerced header cannot grow unbounded.
						if len(ref) > 160 {
							cut := 157
							for cut > 0 && !utf8.RuneStart(ref[cut]) {
								cut--
							}
							ref = ref[:cut] + "..."
						}
						rewrites = append(rewrites, fmt.Sprintf("tools.%s.parameters.$ref(%s)→object", name, ref))
					}
				}
			}
			if paramsObj == nil {
				// Gemini requires a Schema here even when nothing in the
				// caller's parameters survived into a proto-expressible shape.
				paramsObj = map[string]any{"type": "object"}
			}
			decls = append(decls, map[string]any{
				"name":        name,
				"description": desc,
				"parameters":  paramsObj,
			})
			return true
		})
		if schemaErr != nil {
			return provcore.EncodeResult{}, schemaErr
		}
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
			// OpenAI wraps the schema in an envelope
			// (json_schema.{name,strict,schema}); Gemini's responseSchema is
			// the bare Schema proto, and the envelope keys are unknown proto
			// field names that fail the whole request with 400. Unwrap to the
			// inner schema, then sanitize like tool parameters.
			if js := rf.Get("json_schema"); js.Exists() {
				schemaNode := js.Get("schema")
				if !schemaNode.Exists() {
					schemaNode = js
				}
				// Same pipeline and cache as a tool declaration; read-only.
				prepared, err := prepareGeminiSchema([]byte(schemaNode.Raw), false)
				if isSchemaRefFailure(err) {
					// An unresolvable reference sanitizes to {}, leaving
					// responseMimeType asking for JSON with no schema to hold
					// it to — the caller's contract gone, with a 200.
					return provcore.EncodeResult{}, fmt.Errorf("response_format json_schema: %w", err)
				}
				if err == nil && prepared.object {
					gen["responseSchema"] = prepared.encoded
				}
			}
			out["generationConfig"] = gen
		}
	}

	// nexus.ext.gemini.thinking_config is forwarded verbatim into
	// generationConfig.thinkingConfig, merged with keys already populated.
	// Validating the inner subkeys is upstream's job.
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
	} else if cfg := thinkingConfigFromCanonicalEffort(canonicalBody); cfg != nil {
		// Cross-shape: the caller asked to reason in the CANONICAL spelling —
		// `reasoning_effort`, a LEVEL — and this wire takes a BUDGET. Without
		// this, an OpenAI-ingress caller routed to a Gemini model has the
		// intent dropped and the only sign is an answer arriving without the
		// reasoning they asked for, which is the same failure the ingress leg
		// was fixed for.
		//
		// Only the native ext path is skipped, never overridden: a caller who
		// spoke Gemini sent the exact config they wanted.
		gen, _ := out["generationConfig"].(map[string]any)
		if gen == nil {
			gen = map[string]any{}
		}
		gen["thinkingConfig"] = cfg
		out["generationConfig"] = gen
		rewrites = append(rewrites, "reasoning_effort→thinkingConfig.thinkingBudget=-1")
		provdispatch.EmitReasoningPassthrough("gemini", "translated")
	}

	canonicalext.ScanUnsupported("gemini", canonicalBody, geminiSupportedRequestFields)

	body, err := json.Marshal(out)
	if err != nil {
		return provcore.EncodeResult{}, err
	}
	return provcore.EncodeResult{Body: body, ContentType: "application/json", Rewrites: rewrites}, nil
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
			// functionResponse.response must be an object; canonical tool
			// messages carry a string. A JSON object literal forwards
			// as-is, anything else is wrapped as {"result": <value>}.
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
			// Gemini 3 multi-tool turns need the call id echoed back;
			// older models reject the field, so forward only what
			// canonical supplied and never synthesize one here.
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

// The canonical input_audio.format is a bare format name, not a media type.
// The two admitted values are the two OpenAI defines; anything else is refused
// rather than guessed, since a wrong mimeType reaches the model as an
// unreadable attachment and comes back as a confident answer about nothing.
var geminiAudioMime = map[string]string{
	"wav": "audio/wav",
	"mp3": "audio/mp3",
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
			// Only Gemini 3+ accepts functionCall.id on the request body;
			// 1.5 / 2.x reject it as unknown. DecodeResponse still
			// synthesizes an id when Gemini omits it, so OpenAI clients
			// always see a stable tool_call_id.
			// Doc: https://ai.google.dev/gemini-api/docs/function-calling
			if id := call.Get("id").String(); id != "" {
				fc["id"] = id
			}
			part := map[string]any{
				"functionCall": fc,
			}
			// Gemini's provider-native signature belongs to the exact Part that
			// carries this functionCall. Replay only the canonical carrier value.
			if sig := fn.Get("thought_signature").String(); sig != "" {
				part["thoughtSignature"] = sig
			}
			parts = append(parts, part)
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
					media, b64, ok := specutil.ParseDataURL(url)
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
							"mimeType": GuessMimeFromURL(url, "image/jpeg"),
							"fileUri":  url,
						},
					})
				}
			case "video_url":
				// Video rides the same two part shapes as an image. Lifted into
				// content.go beside the other attachment helpers.
				part, perr := videoPart(part.Get("video_url.url").String())
				if perr != nil {
					partsErr = perr
					return false
				}
				parts = append(parts, part)
			case "file":
				// Gemini carries a document with the same two part shapes it
				// uses for an image: inlineData for bytes, fileData for a URI.
				file := part.Get("file")
				switch {
				case strings.HasPrefix(file.Get("file_data").String(), "data:"):
					media, b64, ok := specutil.ParseDataURL(file.Get("file_data").String())
					if !ok {
						partsErr = errUnsupportedField("file.file_data(data:invalid)")
						return false
					}
					parts = append(parts, map[string]any{
						"inlineData": map[string]any{"mimeType": media, "data": b64},
					})
				case file.Get("file_url").Exists():
					uri := file.Get("file_url").String()
					parts = append(parts, map[string]any{
						"fileData": map[string]any{"mimeType": GuessMimeFromURL(uri, "application/octet-stream"), "fileUri": uri},
					})
				default:
					// A bare file_id is an OpenAI-side handle Gemini cannot
					// resolve; an empty part would ask the model about a
					// document it never got.
					partsErr = errUnsupportedField("file(file_id is not resolvable on the Gemini wire)")
					return false
				}
			case "input_audio":
				// Audio rides the same inlineData part as images and
				// documents. Measured against
				// generativelanguage.googleapis.com: every Gemini chat model
				// in the catalog transcribed the fixture WAV. The canonical
				// part carries raw base64 plus a format name, not a data:
				// URL, so the media type is assembled from the format.
				audio := part.Get("input_audio")
				data := audio.Get("data").String()
				if data == "" {
					partsErr = errUnsupportedField("input_audio.data")
					return false
				}
				mime, ok := geminiAudioMime[strings.ToLower(audio.Get("format").String())]
				if !ok {
					partsErr = errUnsupportedField("input_audio.format=" +
						audio.Get("format").String())
					return false
				}
				parts = append(parts, map[string]any{
					"inlineData": map[string]any{"mimeType": mime, "data": data},
				})
			default:
				// A part kind with no case above is REFUSED, never dropped:
				// this branch fires only when the content policy's admitted
				// set and this switch have drifted apart, and a dropped part
				// is indistinguishable from a model ignoring the attachment.
				partsErr = errUnsupportedField("content part '" +
					part.Get("type").String() + "' has no Gemini equivalent")
				return false
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

// The block-walk is delegated to GeminiGenerateNormalizer +
// ProjectToOpenAIChatCompletion — the same parser the audit / compliance /
// agent pipeline uses. The codec retains only the Gemini-specific stamping:
// id ← responseId, model ← modelVersion, finish_reason via MapFinishReason,
// and the usageMetadata extras (cachedContentTokenCount →
// prompt_tokens_details.cached_tokens, thoughtsTokenCount →
// completion_tokens_details.reasoning_tokens). Multi-candidate responses
// become one choices[] entry each, with per-candidate finish_reason.
func (Codec) DecodeResponse(endpoint typology.WireShape, nativeBody []byte, _ string, reqCtx provcore.DecodeContext) (provcore.DecodeResult, error) {
	if endpoint == typology.WireShapeGeminiEmbedContent || endpoint == typology.WireShapeVertexEmbedContent {
		return decodeGeminiEmbeddingResponse(nativeBody, reqCtx.RequestBody)
	}
	if endpoint == typology.WireShapeGeminiImagesGenerateContent {
		return decodeGeminiImagesResponse(nativeBody)
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
	// Mapped before projection so each choices[].finish_reason is correct
	// without a post-process pass.
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
		// Left empty so the projector picks each candidate's per-message
		// reason; first-choice-meta-wins would flatten multi-candidate.
		Usage: UsageToNormalize(usage),
	})
	if err != nil {
		return provcore.DecodeResult{CanonicalBody: nativeBody, Usage: usage}, err
	}

	// Stamped only when Usage was missing the fields — the projector already
	// emits them when Usage carries them.
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

// Returns nil for an empty Usage so the projector omits the "usage" key.
func UsageToNormalize(u provcore.Usage) *normcore.Usage {
	if u.PromptTokens == nil && u.CompletionTokens == nil && u.TotalTokens == nil &&
		u.CacheReadTokens == nil && u.CacheCreationTokens == nil && u.ReasoningTokens == nil {
		return nil
	}
	v := u
	return &v
}

// Newer values (MODEL_ARMOR, UNEXPECTED_TOOL_CALL) fold into the closest
// canonical bucket. Unknown values pass through rather than being lost.
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
	case "OTHER", "":
		return "stop"
	}
	// MALFORMED_FUNCTION_CALL and UNEXPECTED_TOOL_CALL fall through to the
	// raw pass-through arm on purpose: both mean the turn produced NO usable
	// tool call. Mapping them to "tool_calls" sends an agent loop looking for
	// a tool_calls[] that is empty, where it stalls or loops. The canonical
	// enum has no value for "the model failed to produce a call".
	return r
}
