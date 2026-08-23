package cohere

import (
	"fmt"
	"github.com/goccy/go-json"
	"strings"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// codec implements provcore.SchemaCodec for Cohere's Chat v2 API.
//
// EncodeRequest projects the canonical body onto the field set Cohere v2
// actually accepts, renaming the canonical spellings that differ (top_p → p,
// top_k → k, stop → stop_sequences, max_completion_tokens → max_tokens). See
// codec_request_fields.go for the measured set and for why this is a projection
// rather than the passthrough-with-exceptions it used to be.
//
// DecodeResponse converts Cohere's response shape back to canonical
// OpenAI chat-completions:
//
//	Cohere:   {id, message: {role, content: [{type:"text", text:"..."}],
//	            tool_plan, tool_calls}, finish_reason, usage:{tokens:{...}}}
//	OpenAI:   {id, model, object:"chat.completion", choices: [{index,
//	            message: {role, content, tool_calls}, finish_reason}],
//	            usage: {prompt_tokens, completion_tokens, total_tokens}}
//
// Cohere's `tool_plan` (the model's reasoning trace before invoking
// tools) has no direct OpenAI canonical slot. We surface it via the
// non-standard but increasingly-common `delta.reasoning_content` /
// `message.reasoning_content` field so OpenAI o-series clients
// already wired for that field can see it.
type codec struct{}

// EncodeRequest reduces the canonical body to the Cohere v2 wire shape.
func (codec) EncodeRequest(endpoint typology.WireShape, canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	switch endpoint {
	case typology.WireShapeCohereChat:
		// Fall through to chat logic below.
	case typology.WireShapeCohereEmbed:
		return canonicalToCohereEmbed(canonicalBody, target)
	case typology.WireShapeCohereRerank:
		return encodeCohereRerank(canonicalBody, target)
	default:
		return provcore.EncodeResult{}, fmt.Errorf("cohere: unsupported endpoint %q", endpoint)
	}
	if len(canonicalBody) == 0 {
		return provcore.EncodeResult{ContentType: "application/json"}, nil
	}
	if !gjson.ValidBytes(canonicalBody) {
		return provcore.EncodeResult{}, fmt.Errorf("cohere: invalid canonical body")
	}
	// Content first: a file part has to leave the message array before the
	// projection runs, because `documents` is a top-level field the projection
	// carries and `messages` is one it forwards verbatim.
	lifted, err := translateContentForCohereChat(canonicalBody)
	if err != nil {
		return provcore.EncodeResult{}, err
	}
	body := projectToCohereChat(lifted)
	// The model this wire is told to call is the one ROUTING resolved. The
	// canonical body carries the caller's word — often "auto", or another
	// provider's name — and no upstream knows it, so forwarding it buys a 404
	// naming a model nobody asked for. A target that resolved nothing leaves
	// the caller's word alone, which is how a caller addressing this provider
	// directly still reaches it.
	// The same stamp the native door applies, for the same reason it states:
	// an unresolved model in the body root is a 404 from Cohere naming a model
	// nobody asked for.
	if target.ProviderModelID != "" {
		out, err := provcore.SurgicalModelStamp(body, target.ProviderModelID)
		if err != nil {
			return provcore.EncodeResult{}, err
		}
		return provcore.EncodeResult{Body: out, ContentType: "application/json"}, nil
	}
	return provcore.EncodeResult{Body: body, ContentType: "application/json"}, nil
}

// DecodeResponse converts Cohere v2 response to canonical OpenAI shape.
func (codec) DecodeResponse(endpoint typology.WireShape, nativeBody []byte, _ string, reqCtx provcore.DecodeContext) (provcore.DecodeResult, error) {
	if endpoint == typology.WireShapeCohereEmbed {
		return cohereEmbedResponseToCanonical(nativeBody, reqCtx.RequestBody)
	}
	if endpoint == typology.WireShapeCohereRerank {
		return decodeCohereRerankResponse(nativeBody)
	}
	if endpoint != typology.WireShapeCohereChat {
		return provcore.DecodeResult{CanonicalBody: nativeBody}, nil
	}
	if len(nativeBody) == 0 {
		return provcore.DecodeResult{CanonicalBody: nativeBody}, nil
	}
	if !gjson.ValidBytes(nativeBody) {
		return provcore.DecodeResult{}, fmt.Errorf("cohere: invalid response body")
	}

	// Build the canonical content string from the message.content blocks.
	var content strings.Builder
	gjson.GetBytes(nativeBody, "message.content").ForEach(func(_, part gjson.Result) bool {
		if part.Get("type").Str == "text" {
			content.WriteString(part.Get("text").Str)
		}
		return true
	})

	message := map[string]any{
		"role":    "assistant",
		"content": content.String(),
	}
	if plan := gjson.GetBytes(nativeBody, "message.tool_plan"); plan.Type == gjson.String && plan.Str != "" {
		// Surface Cohere's reasoning trace via OpenAI's reasoning_content
		// extension so o-series-aware clients can consume it.
		message["reasoning_content"] = plan.Str
	}
	if tc := gjson.GetBytes(nativeBody, "message.tool_calls"); tc.IsArray() {
		var calls []json.RawMessage
		tc.ForEach(func(_, call gjson.Result) bool {
			calls = append(calls, json.RawMessage(call.Raw))
			return true
		})
		message["tool_calls"] = calls
	}

	finishReason := gjson.GetBytes(nativeBody, "finish_reason").Str
	if finishReason == "" {
		finishReason = "stop"
	}

	// Cohere's chat response carries no model field, so reading it off the
	// body yields "" — and an auto-routed caller then cannot see from the
	// body which model answered. The resolved target is the model the call
	// was MADE with; the body's own value still wins if a future API
	// version starts reporting one.
	model := gjson.GetBytes(nativeBody, "model").Str
	if model == "" {
		model = reqCtx.Target.ProviderModelID
	}

	canonical := map[string]any{
		"id":      gjson.GetBytes(nativeBody, "id").Str,
		"object":  "chat.completion",
		"created": 0,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}

	// Usage extraction via provcore.ExtractUsage (shared/normalize Tier-1
	// normalizer). Handles both usage.tokens.* and usage.billed_units.* fallback.
	usage := provcore.ExtractUsage(nativeBody, provcore.FormatCohere)
	if usage.PromptTokens != nil || usage.CompletionTokens != nil {
		// Inject usage block into canonical body so the executor reports it.
		usageMap := map[string]any{}
		if usage.PromptTokens != nil {
			usageMap["prompt_tokens"] = *usage.PromptTokens
		}
		if usage.CompletionTokens != nil {
			usageMap["completion_tokens"] = *usage.CompletionTokens
		}
		if usage.TotalTokens != nil {
			usageMap["total_tokens"] = *usage.TotalTokens
		}
		canonical["usage"] = usageMap
	}

	body, err := json.Marshal(canonical)
	if err != nil {
		return provcore.DecodeResult{}, err
	}
	return provcore.DecodeResult{CanonicalBody: body, Usage: usage}, nil
}
