// Package openai — bridge.go exposes helpers from the openai sub-packages
// (codec, errors, rewrites, responses, stream) as package-level functions so
// sibling adapters (Azure, compat/*) and cross-cutting callers (canonicalbridge,
// builtins) can reach them with a single import.
package openai

import (
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/codec"
	specerrors "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/errors"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/responses"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/rewrites"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/stream"
)

// IdentityCodec returns the OpenAI identity SchemaCodec, shared by all
// OpenAI-compat adapters (DeepSeek, Azure, Groq, Together, …).
func IdentityCodec() provcore.SchemaCodec { return codec.IdentityCodec() }

// ErrorNormalizerInstance returns the shared OpenAI-style error normaliser
// for use by OpenAI-compat sibling adapters.
func ErrorNormalizerInstance() provcore.ErrorNormalizer { return specerrors.ErrorNormalizer{} }

// NewStreamDecoder returns a StreamDecoder for OpenAI-format SSE streams.
// Shared by all OpenAI-compat adapters.
func NewStreamDecoder(log *slog.Logger) *stream.StreamDecoder {
	return stream.NewStreamDecoder(log)
}

// NewResponsesStreamDecoder returns a ResponsesStreamDecoder for
// OpenAI Responses-API SSE streams.
func NewResponsesStreamDecoder(log *slog.Logger) *stream.ResponsesStreamDecoder {
	return stream.NewResponsesStreamDecoder(log)
}

// ApplyReasoningRewrites is the PassthroughRewrite callback for OpenAI
// reasoning models (o-series, gpt-5). Shared by Azure OpenAI adapter.
var ApplyReasoningRewrites = rewrites.ApplyReasoningRewrites

// IsReasoningModel reports whether modelID belongs to an OpenAI reasoning
// model family (o-series, gpt-5). Exposed for tests and any internal callers.
func IsReasoningModel(modelID string) bool {
	return rewrites.IsReasoningModel(modelID)
}

// IsResponsesBuiltinTool reports whether the given tool type name is an
// OpenAI-native Responses-API built-in tool. Used by cross_format.go.
func IsResponsesBuiltinTool(typeStr string) bool {
	return responses.IsResponsesBuiltinTool(typeStr)
}

// DecodeResponsesRequest converts a Responses-API request body into
// canonical chat-completions shape. Used by canonicalbridge.
func DecodeResponsesRequest(raw []byte) ([]byte, error) {
	return responses.DecodeResponsesRequest(raw)
}

// EncodeResponsesRequest converts a canonical chat-completions body into
// Responses-API request shape (inverse of DecodeResponsesRequest). Exposed
// for canonicalbridge round-trip tests.
func EncodeResponsesRequest(canonical []byte) ([]byte, error) {
	return responses.EncodeResponsesRequest(canonical)
}

// DecodeResponsesResponse converts a Responses-API response body into
// canonical chat-completions shape + Usage. Used by proxy.go.
func DecodeResponsesResponse(raw []byte) ([]byte, provcore.Usage, error) {
	return responses.DecodeResponsesResponse(raw)
}

// EncodeResponsesResponse converts a canonical chat-completions response into
// Responses-API response shape. Used by canonicalbridge and proxy.go.
func EncodeResponsesResponse(canonical []byte, requestID, modelOverride string) ([]byte, error) {
	return responses.EncodeResponsesResponse(canonical, requestID, modelOverride)
}
