// Package gemini implements the traffic adapter for the Google Gemini
// API (`generativelanguage.googleapis.com` /v1beta/models/...:generateContent
// and :streamGenerateContent). Extracts content from system instructions,
// conversation parts, function calls, thinking parts, and streaming
// responses.
package gemini

import (
	"context"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// requestKnownKeys lists known top-level fields on a Gemini
// generateContent request body. Anything else lands in Extra so a
// future spec field (cached content extension, new tool variants,
// citations grounding additions, …) reaches compliance hooks.
var requestKnownKeys = []string{
	"contents", "systemInstruction", "system_instruction",
	"generationConfig", "safetySettings", "tools", "toolConfig",
	"cachedContent", "model", "labels", "allowedFunctionNames",
}

// responseKnownKeys lists known top-level fields on a non-streaming
// Gemini response.
var responseKnownKeys = []string{
	"candidates", "promptFeedback", "usageMetadata", "modelVersion",
	"responseId", "automaticFunctionCallingHistory",
}

// Adapter implements the Gemini content extraction.
type Adapter struct{}

// ID returns the adapter identifier.
func (a *Adapter) ID() string { return "gemini" }

// Configure is a no-op for the gemini adapter (no custom config needed).
func (a *Adapter) Configure(_ map[string]any) error { return nil }

// ExtractRequest parses a Gemini generateContent request body.
//
// Surfaces every audit-relevant slot:
//   - System instruction parts → Segments
//   - contents[].parts[].text → Segments
//   - contents[].parts[] with thought=true (Gemini 2.5+ extended
//     thinking carried in conversation history) → ReasoningSegments
//   - contents[].parts[].functionCall → ToolCallSegments (assistant
//     function call echoed in conversation history)
//   - contents[].parts[].functionResponse → Segments (tool return text;
//     compliance scans tool returns for PII)
//   - Top-level `tools` (function declarations) → Metadata
//   - Top-level `model`, `cachedContent` → Metadata
//   - Anything else → Extra
func (a *Adapter) ExtractRequest(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	contents := gjson.GetBytes(body, "contents")
	if !contents.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	var segments, reasoning, toolCalls []string

	// System instruction parts (camelCase newer or snake_case older).
	for _, key := range []string{"systemInstruction.parts", "system_instruction.parts"} {
		sys := gjson.GetBytes(body, key)
		if sys.IsArray() {
			sys.ForEach(func(_, part gjson.Result) bool {
				if text := part.Get("text"); text.Type == gjson.String {
					segments = append(segments, text.Str)
				}
				return true
			})
		}
	}

	// contents[].parts[]:
	//   text             → Segments (or ReasoningSegments when thought=true)
	//   functionCall     → ToolCallSegments
	//   functionResponse → Segments (string or {result: string})
	contents.ForEach(func(_, content gjson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		parts.ForEach(func(_, part gjson.Result) bool {
			// Thinking content: Gemini 2.5+ marks reasoning parts with
			// thought=true. Keep them off Segments to mirror Anthropic
			// thinking + OpenAI reasoning_content semantics.
			if text := part.Get("text"); text.Type == gjson.String {
				if part.Get("thought").Bool() {
					if text.Str != "" {
						reasoning = append(reasoning, text.Str)
					}
				} else {
					segments = append(segments, text.Str)
				}
				return true
			}
			if fc := part.Get("functionCall"); fc.IsObject() {
				toolCalls = append(toolCalls, fc.Raw)
				return true
			}
			if fr := part.Get("functionResponse"); fr.Exists() {
				resp := fr.Get("response")
				switch {
				case resp.Type == gjson.String:
					segments = append(segments, resp.Str)
				case resp.IsObject():
					if r := resp.Get("result"); r.Exists() && r.Type == gjson.String {
						segments = append(segments, r.Str)
					}
				}
			}
			return true
		})
		return true
	})

	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Exists() && model.Type == gjson.String {
		meta["model"] = model.Str
	}
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		meta["tools"] = tools.Raw
	}
	if cached := gjson.GetBytes(body, "cachedContent"); cached.Exists() && cached.Type == gjson.String && cached.Str != "" {
		meta["cachedContent"] = cached.Str
	}

	return traffic.NormalizedContent{
		Segments:          segments,
		ReasoningSegments: reasoning,
		ToolCallSegments:  toolCalls,
		Metadata:          meta,
		Extra:             traffic.CollectExtra(body, requestKnownKeys),
	}, nil
}

// ExtractResponse parses a Gemini generateContent response body.
//
//   - candidates[].content.parts[].text                  → Segments
//     (or ReasoningSegments when thought=true)
//   - candidates[].content.parts[].functionCall          → ToolCallSegments
//   - candidates[].finishReason (joined across candidates) → Metadata
//   - top-level modelVersion / responseId                → Metadata
//   - Anything else top-level → Extra
func (a *Adapter) ExtractResponse(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	candidates := gjson.GetBytes(body, "candidates")
	if !candidates.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	var segments, reasoning, toolCalls []string
	finishReasons := []string{}
	candidates.ForEach(func(_, candidate gjson.Result) bool {
		parts := candidate.Get("content.parts")
		if parts.IsArray() {
			parts.ForEach(func(_, part gjson.Result) bool {
				if text := part.Get("text"); text.Type == gjson.String {
					if part.Get("thought").Bool() {
						if text.Str != "" {
							reasoning = append(reasoning, text.Str)
						}
					} else {
						segments = append(segments, text.Str)
					}
					return true
				}
				if fc := part.Get("functionCall"); fc.IsObject() {
					toolCalls = append(toolCalls, fc.Raw)
				}
				return true
			})
		}
		if fr := candidate.Get("finishReason"); fr.Exists() && fr.Type == gjson.String && fr.Str != "" {
			finishReasons = append(finishReasons, fr.Str)
		}
		return true
	})

	meta := map[string]string{}
	if mv := gjson.GetBytes(body, "modelVersion"); mv.Exists() && mv.Type == gjson.String && mv.Str != "" {
		meta["modelVersion"] = mv.Str
	}
	if rid := gjson.GetBytes(body, "responseId"); rid.Exists() && rid.Type == gjson.String && rid.Str != "" {
		meta["responseId"] = rid.Str
	}
	if len(finishReasons) > 0 {
		meta["finishReason"] = strings.Join(finishReasons, ",")
	}

	return traffic.NormalizedContent{
		Segments:          segments,
		ReasoningSegments: reasoning,
		ToolCallSegments:  toolCalls,
		Metadata:          meta,
		Extra:             traffic.CollectExtra(body, responseKnownKeys),
	}, nil
}

// ExtractStreamChunk parses a Gemini streaming response chunk. Gemini
// streams full candidate objects per chunk, so the format is identical
// to a non-streaming response — we delegate to the same extraction
// logic used by ExtractResponse but skip the Extra walk (chunks are
// frequent and the per-chunk Extra noise is not useful).
func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	var segments, reasoning, toolCalls []string
	finishReasons := []string{}
	candidates := gjson.GetBytes(chunk, "candidates")
	if candidates.IsArray() {
		candidates.ForEach(func(_, candidate gjson.Result) bool {
			parts := candidate.Get("content.parts")
			if parts.IsArray() {
				parts.ForEach(func(_, part gjson.Result) bool {
					if text := part.Get("text"); text.Type == gjson.String && text.Str != "" {
						if part.Get("thought").Bool() {
							reasoning = append(reasoning, text.Str)
						} else {
							segments = append(segments, text.Str)
						}
						return true
					}
					if fc := part.Get("functionCall"); fc.IsObject() {
						toolCalls = append(toolCalls, fc.Raw)
					}
					return true
				})
			}
			if fr := candidate.Get("finishReason"); fr.Exists() && fr.Type == gjson.String && fr.Str != "" {
				finishReasons = append(finishReasons, fr.Str)
			}
			return true
		})
	}

	var meta map[string]string
	if len(finishReasons) > 0 {
		meta = map[string]string{"finishReason": strings.Join(finishReasons, ",")}
	}

	return traffic.NormalizedContent{
		Segments:          segments,
		ReasoningSegments: reasoning,
		ToolCallSegments:  toolCalls,
		Metadata:          meta,
	}, nil
}
