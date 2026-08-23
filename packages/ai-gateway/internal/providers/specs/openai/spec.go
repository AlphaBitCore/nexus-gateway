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
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

// NewSpec returns a fully wired OpenAI [provcore.AdapterSpec].
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:    provcore.FormatOpenAI,
		Transport: NewTransport(log),
		// The contract carries OpenAI's per-model wire rules (reasoning
		// max_tokens rename + sampling strips on both chat and responses
		// wires; ada-002 embedding strips) into both codec entry points —
		// no dispatch-level rewrite callback.
		// Gated: the wire's content-part limits are declared in codec_content.go
		// and enforced on both codec doors, so a document it cannot carry as a
		// document rides as text and a part it has no variant for is refused in
		// our words rather than in OpenAI's deserializer vocabulary.
		SchemaCodec:     specutil.GateContent(codec.New(rewrites.OpenAIContract()), contentPolicyFor),
		StreamDecoder:   stream.NewStreamDecoder(log),
		ErrorNormalizer: specerrors.ErrorNormalizer{},
		// OpenAI natively serves chat-completions, responses-api, and embeddings.
		// Any sibling (Moonshot, Groq, Together, ...) needs its own captured-200
		// evidence before declaring "responses-api" per
		// provider-adapter-architecture.md §3a Rule 7.
		RequestShapes: []typology.WireShape{typology.WireShapeOpenAIChat, typology.WireShapeOpenAIResponses, typology.WireShapeOpenAIEmbeddings},
	}
}
