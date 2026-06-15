package replicate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// codec implements provcore.SchemaCodec for Replicate's prediction
// API.
//
// EncodeRequest maps a canonical OpenAI chat-completions body into the
// Replicate request shape:
//
//	{
//	  "version": "<provider-model-id>",
//	  "stream":  true,
//	  "input": {
//	    "prompt":   "<combined system + user messages>",
//	    "messages": <original OpenAI messages array>
//	  }
//	}
//
// We pass both `prompt` (a flattened text) and `messages` (the
// structured array) so the underlying Replicate model can pick whichever
// input shape matches its schema (`prompt` for legacy chat models,
// `messages` for newer chat-tuned ones).
//
// DecodeResponse maps the Replicate prediction-result shape back into
// a canonical OpenAI chat-completions response. Replicate's `output`
// field is task-specific:
//   - string  → choices[0].message.content
//   - array   → joined into a single string (concatenated, no separator)
//   - object  → look for text/answer/completion/message keys
//
// Replicate does not surface OpenAI-style usage tokens; usage stays
// empty when the response has no `metrics` section, otherwise we
// populate from `metrics.input_token_count` /
// `metrics.output_token_count` when present (some hosted Llama models
// return these).
type codec struct{}

// EncodeRequest converts canonical OpenAI body to Replicate prediction body.
func (codec) EncodeRequest(endpoint typology.WireShape, canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	if endpoint != typology.WireShapeOpenAIChat {
		return provcore.EncodeResult{}, fmt.Errorf("replicate: unsupported endpoint %q", endpoint)
	}
	if len(canonicalBody) == 0 {
		return provcore.EncodeResult{ContentType: "application/json"}, nil
	}
	if !gjson.ValidBytes(canonicalBody) {
		return provcore.EncodeResult{}, fmt.Errorf("replicate: invalid canonical body")
	}

	// Flatten messages into a single prompt string, keeping role tags
	// so models that accept only a `prompt` field still see role context.
	var prompt strings.Builder
	gjson.GetBytes(canonicalBody, "messages").ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").Str
		content := msg.Get("content")
		if content.Type == gjson.String {
			fmt.Fprintf(&prompt, "%s: %s\n", role, content.Str)
		} else if content.IsArray() {
			content.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").Str == "text" {
					fmt.Fprintf(&prompt, "%s: %s\n", role, part.Get("text").Str)
				}
				return true
			})
		}
		return true
	})

	input := map[string]any{
		"prompt":   strings.TrimSpace(prompt.String()),
		"messages": json.RawMessage(gjson.GetBytes(canonicalBody, "messages").Raw),
	}
	if v := gjson.GetBytes(canonicalBody, "max_tokens"); v.Exists() && v.Type == gjson.Number {
		input["max_tokens"] = v.Int()
	}
	if v := gjson.GetBytes(canonicalBody, "temperature"); v.Exists() && v.Type == gjson.Number {
		input["temperature"] = v.Float()
	}
	if v := gjson.GetBytes(canonicalBody, "top_p"); v.Exists() && v.Type == gjson.Number {
		input["top_p"] = v.Float()
	}

	envelope := map[string]any{
		"version": target.ProviderModelID,
		"stream":  gjson.GetBytes(canonicalBody, "stream").Bool(),
		"input":   input,
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return provcore.EncodeResult{}, err
	}
	return provcore.EncodeResult{Body: body, ContentType: "application/json"}, nil
}

// DecodeResponse converts Replicate prediction-result body to canonical
// OpenAI chat-completions response.
func (codec) DecodeResponse(endpoint typology.WireShape, nativeBody []byte, _ string, _ provcore.DecodeContext) (provcore.DecodeResult, error) {
	if endpoint != typology.WireShapeOpenAIChat {
		return provcore.DecodeResult{}, fmt.Errorf("replicate: unsupported endpoint %q", endpoint)
	}
	if len(nativeBody) == 0 {
		return provcore.DecodeResult{CanonicalBody: nativeBody}, nil
	}
	if !gjson.ValidBytes(nativeBody) {
		return provcore.DecodeResult{}, fmt.Errorf("replicate: invalid response body")
	}

	out := gjson.GetBytes(nativeBody, "output")
	var content string
	switch {
	case out.Type == gjson.String:
		content = out.Str
	case out.IsArray():
		var b strings.Builder
		out.ForEach(func(_, item gjson.Result) bool {
			if item.Type == gjson.String {
				b.WriteString(item.Str)
			}
			return true
		})
		content = b.String()
	case out.IsObject():
		for _, key := range []string{"text", "answer", "completion", "message"} {
			if v := out.Get(key); v.Type == gjson.String && v.Str != "" {
				content = v.Str
				break
			}
		}
	}

	finishReason := "stop"
	if status := gjson.GetBytes(nativeBody, "status"); status.Type == gjson.String {
		switch status.Str {
		case "failed", "canceled":
			finishReason = "error"
		}
	}
	if e := gjson.GetBytes(nativeBody, "error"); e.Type == gjson.String && e.Str != "" {
		// Surface error text into content so the caller sees the failure
		// message in the assistant message field.
		if content == "" {
			content = e.Str
		}
		finishReason = "error"
	}

	id := gjson.GetBytes(nativeBody, "id").Str
	model := gjson.GetBytes(nativeBody, "version").Str

	canonical := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": gjson.GetBytes(nativeBody, "created_at").Time().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": finishReason,
			},
		},
	}

	// Usage extraction via provcore.ExtractUsage (shared/normalize Tier-1 normalizer).
	usage := provcore.ExtractUsage(nativeBody, provcore.FormatReplicate)

	body, err := json.Marshal(canonical)
	if err != nil {
		return provcore.DecodeResult{}, err
	}
	return provcore.DecodeResult{CanonicalBody: body, Usage: usage}, nil
}
