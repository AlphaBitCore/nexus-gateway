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

// Contract aliases the identity codec's wire-rule contract so sibling
// adapters construct their codecs with a single openai import.
type Contract = codec.Contract

// NewIdentityCodec returns an OpenAI-family identity SchemaCodec carrying
// the sibling's wire-rule contract — a required argument by design (see
// codec.New). Siblings with no probed wire quirk pass the zero Contract.
func NewIdentityCodec(c Contract) provcore.SchemaCodec { return codec.New(c) }

// OpenAIContract returns the OpenAI wire-rule contract (reasoning-model
// parameter quirks, ada-002 embedding strips). Shared verbatim by the
// Azure OpenAI adapter: Azure serves the same model families behind
// deployment URLs and returns the same wire 400s.
func OpenAIContract() Contract { return rewrites.OpenAIContract() }

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
