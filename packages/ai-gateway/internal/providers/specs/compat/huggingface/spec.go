// Package spec_huggingface wires the Hugging Face provider AdapterSpec.
//
// Hugging Face exposes two distinct surfaces:
//
//   - Text Generation Inference (TGI) endpoints — both api-inference.huggingface.co
//     and customer-deployed Inference Endpoints expose an OpenAI-compatible
//     `/v1/chat/completions` route. This codec assumes the TGI surface
//     because that is what enterprise customers wire through ai-gateway
//     for LLM forwarding. The transport / codec / stream / errors are
//     all reused from spec_openai because the wire format matches.
//   - Legacy serverless `/models/<model>` endpoints expose task-specific
//     wire shapes (text-generation, summarization, fill-mask, etc.).
//     Those are NOT covered by this ai-gateway codec — admins who need
//     to forward legacy serverless traffic should configure the Provider
//     with adapterType "openai" only when their endpoint exposes a TGI
//     OpenAI-compat route. Compliance proxy / agent traffic capture for
//     legacy serverless is still covered by the huggingface traffic
//     adapter (`packages/shared/traffic/adapters/huggingface/`).
package huggingface

import (
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
)

// NewSpec returns the Hugging Face [provcore.AdapterSpec].
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:          provcore.FormatHuggingFace,
		Transport:       openai.NewTransport(log),
		SchemaCodec:     openai.IdentityCodec(),
		StreamDecoder:   openai.NewStreamDecoder(log),
		ErrorNormalizer: openai.ErrorNormalizerInstance(),
	}
}
