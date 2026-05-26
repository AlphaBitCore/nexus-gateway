// Package spec_openai wires the OpenAI provider AdapterSpec. OpenAI
// is the canonical wire format, so the SchemaCodec is effectively the
// identity — canonical in, canonical out. The Transport and
// ErrorNormalizer still matter because the gateway must probe OpenAI,
// classify its rate-limit errors, and re-issue Authorization headers.
package openai

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/codec"
	specerrors "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/errors"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/rewrites"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/stream"
)

// NewSpec returns a fully wired OpenAI [provcore.AdapterSpec].
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:             provcore.FormatOpenAI,
		Transport:          NewTransport(log),
		SchemaCodec:        codec.IdentityCodec(),
		StreamDecoder:      stream.NewStreamDecoder(log),
		ErrorNormalizer:    specerrors.ErrorNormalizer{},
		PassthroughRewrite: rewrites.ApplyReasoningRewrites,
		// OpenAI natively serves chat-completions, responses-api, and embeddings.
		// Any sibling (Moonshot, Groq, Together, ...) needs its own captured-200
		// evidence before declaring "responses-api" per
		// provider-adapter-architecture.md §3a Rule 7.
		// The IdentityCodec applies per-model wire rules for the embeddings
		// endpoint (ada-002 dimension/encoding_format strip; text-embedding-3-*
		// dimension safety-net).
		RequestShapes: []typology.WireShape{typology.WireShapeOpenAIChat, typology.WireShapeOpenAIResponses, typology.WireShapeOpenAIEmbeddings},
	}
}
