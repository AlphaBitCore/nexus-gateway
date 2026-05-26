// Package codec implements the OpenAI identity SchemaCodec. It is an
// internal sub-package of specs/openai; the root package re-exports
// IdentityCodec() and ErrorNormalizerInstance() via bridge.go.
package codec

import (
	"fmt"

	specerrors "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/errors"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// IdentityCodec returns a SchemaCodec shared with OpenAI-compat
// providers (DeepSeek, GLM openai-compat, Azure OpenAI) that already
// speak the canonical OpenAI shape on the wire.
func IdentityCodec() provcore.SchemaCodec { return identityCodec{} }

// ErrorNormalizerInstance returns the shared OpenAI-style error
// normaliser so OpenAI-compat sibling adapters (DeepSeek, Azure) can
// reuse it without importing unexported symbols.
func ErrorNormalizerInstance() provcore.ErrorNormalizer { return specerrors.ErrorNormalizer{} }

// identityCodec is the SchemaCodec for OpenAI. OpenAI is the canonical
// wire format, so EncodeRequest is a no-op for chat/completions (the
// specAdapter uses passthrough when BodyFormat == Format already; this
// path fires only when an explicit canonical → canonical translation is
// requested). For EndpointEmbeddings it applies per-model wire rules
// per provider-adapter-architecture.md §3a Rule 3.
//
// DecodeResponse is identity for all endpoints; its only real job is to
// extract token usage so the executor's ExecutionResult.Usage is populated
// consistently across every OpenAI-compat sibling. Usage extraction
// delegates to provcore.ExtractUsage (shared/normalize Tier-1 normalizer).
// The alias chain (Kimi flat cached_tokens, DeepSeek prompt_cache_hit_tokens,
// Moonshot prompt_cache_tokens, OpenAI Responses-shape fallbacks) lives in
// shared/normalize/openai_chat.go's extractCanonicalUsage method.
type identityCodec struct{}

func (identityCodec) EncodeRequest(endpoint typology.WireShape, canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	switch endpoint {
	case typology.WireShapeOpenAIChat, typology.WireShapeOpenAIResponses:
		return provcore.EncodeResult{Body: canonicalBody, ContentType: "application/json"}, nil
	case typology.WireShapeOpenAIEmbeddings:
		return encodeOpenAIEmbeddingRequest(canonicalBody, target)
	case typology.WireShapeNone, typology.WireShapeOpenAICompletionsLegacy:
		return provcore.EncodeResult{Body: canonicalBody, ContentType: "application/json"}, nil
	}
	return provcore.EncodeResult{}, fmt.Errorf("openai: unsupported endpoint %q", endpoint)
}

// DecodeResponse is identity for all endpoints. OpenAI response shape IS
// the canonical shape, so no structural translation is needed. For
// EndpointEmbeddings the response is forwarded verbatim; the canonical
// bridge layer handles any ingress-format projection upstream. Usage is
// extracted via the shared Tier-1 normalizer so the audit pipeline sees
// consistent token counts.
func (identityCodec) DecodeResponse(_ typology.WireShape, nativeBody []byte, _ string) (provcore.DecodeResult, error) {
	return provcore.DecodeResult{
		CanonicalBody: nativeBody,
		Usage:         provcore.ExtractUsage(nativeBody, provcore.FormatOpenAI),
	}, nil
}
