// Package spec_anthropic wires the Anthropic Messages provider
// AdapterSpec. Anthropic uses a distinct wire schema (system prompt at
// top-level, content blocks, `messages` endpoint `/v1/messages`) that
// the SchemaCodec translates to and from the canonical OpenAI shape.
package anthropic

import (
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	apcodec "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/codec"
	specerrors "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/errors"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/ingress"
	apstream "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/stream"
)

// NewSpec returns the Anthropic [provcore.AdapterSpec].
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:          provcore.FormatAnthropic,
		Transport:       NewTransport(log),
		SchemaCodec:     apcodec.NewCodec(),
		StreamDecoder:   apstream.NewStreamDecoder(log),
		ErrorNormalizer: specerrors.ErrorNormalizer{},
	}
}

// NewStreamDecoder returns a StreamDecoder for Anthropic-format SSE streams.
func NewStreamDecoder(log *slog.Logger) *apstream.StreamDecoder {
	return apstream.NewStreamDecoder(log)
}

// MessagesRequestToOpenAIChatCompletion converts an Anthropic Messages API
// request body into canonical OpenAI chat.completions JSON. Used by
// canonicalbridge (hub-ingress path).
func MessagesRequestToOpenAIChatCompletion(native []byte, providerModelID string) ([]byte, error) {
	return ingress.MessagesRequestToOpenAIChatCompletion(native, providerModelID)
}

// OpenAIChatCompletionToMessagesResponse converts a canonical OpenAI
// chat.completion JSON body into an Anthropic Messages API response shape.
// Used by canonicalbridge (hub-egress path).
func OpenAIChatCompletionToMessagesResponse(openaiBody []byte) ([]byte, error) {
	return ingress.OpenAIChatCompletionToMessagesResponse(openaiBody)
}
