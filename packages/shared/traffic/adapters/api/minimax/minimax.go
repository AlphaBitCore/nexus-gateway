// Package minimax implements the traffic adapter for the MiniMax API.
// Supports both MiniMax native format (prompt/text/sender_type) and
// OpenAI-compatible format (messages/content/role).
package minimax

import (
	"context"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Adapter implements the MiniMax content extraction.
type Adapter struct{}

// ID returns the adapter identifier.
func (a *Adapter) ID() string { return "minimax" }

// Configure is a no-op for the minimax adapter (no custom config needed).
func (a *Adapter) Configure(_ map[string]any) error { return nil }

// ExtractRequest parses a MiniMax request body. Detects native vs OpenAI-compat
// format by checking whether the first message has a "text" or "content" field.
func (a *Adapter) ExtractRequest(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	var segments []string

	// Extract system prompt (native format).
	prompt := gjson.GetBytes(body, "prompt")
	if prompt.Exists() && prompt.Type == gjson.String && prompt.Str != "" {
		segments = append(segments, prompt.Str)
	}

	// Detect format from first message: "text" = native, "content" = compat.
	first := messages.Array()
	if len(first) == 0 {
		return traffic.NormalizedContent{Segments: segments, Metadata: extractMiniMaxMeta(body)}, nil
	}

	isNative := first[0].Get("text").Exists()

	messages.ForEach(func(_, msg gjson.Result) bool {
		if isNative {
			text := msg.Get("text")
			if text.Exists() && text.Type == gjson.String {
				segments = append(segments, text.Str)
			}
		} else {
			content := msg.Get("content")
			if content.Exists() && content.Type == gjson.String {
				segments = append(segments, content.Str)
			}
		}
		return true
	})

	return traffic.NormalizedContent{Segments: segments, Metadata: extractMiniMaxMeta(body)}, nil
}

// ExtractResponse parses a MiniMax response body.
// Supports both native (text) and compat (content) formats.
func (a *Adapter) ExtractResponse(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	choices := gjson.GetBytes(body, "choices")
	if !choices.Exists() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	var segments []string
	choices.ForEach(func(_, choice gjson.Result) bool {
		// Try native format first (message.text), then compat (message.content).
		text := choice.Get("message.text")
		if text.Exists() && text.Type == gjson.String {
			segments = append(segments, text.Str)
			return true
		}
		content := choice.Get("message.content")
		if content.Exists() && content.Type == gjson.String {
			segments = append(segments, content.Str)
		}
		return true
	})

	return traffic.NormalizedContent{Segments: segments}, nil
}

// ExtractStreamChunk parses a MiniMax streaming SSE chunk.
// Supports both native (delta.text) and compat (delta.content) formats.
func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, _ string) (traffic.NormalizedContent, error) {
	if !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}

	var segments []string

	// Try native format first.
	text := gjson.GetBytes(chunk, "choices.0.delta.text")
	if text.Exists() && text.Type == gjson.String && text.Str != "" {
		segments = append(segments, text.Str)
		return traffic.NormalizedContent{Segments: segments}, nil
	}

	// Fall back to compat format.
	content := gjson.GetBytes(chunk, "choices.0.delta.content")
	if content.Exists() && content.Type == gjson.String && content.Str != "" {
		segments = append(segments, content.Str)
	}

	return traffic.NormalizedContent{Segments: segments}, nil
}

func extractMiniMaxMeta(body []byte) map[string]string {
	meta := map[string]string{}
	if model := gjson.GetBytes(body, "model"); model.Exists() {
		meta["model"] = model.Str
	}
	return meta
}
