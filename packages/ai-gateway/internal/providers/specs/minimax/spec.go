// Package spec_minimax wires the MiniMax AdapterSpec. MiniMax now markets
// the OpenAI-compatible `/v1/chat/completions` endpoint at
// api.minimax.io (international) / api.minimaxi.com (mainland) as the
// primary surface for the M2/M2.5/M2.7 model family — the legacy
// `/v1/text/chatcompletion_pro` body schema (sender_type / bot_setting /
// reply_constraints) is retired here. We share spec_openai's
// IdentityCodec, stream decoder, and error normaliser; the only MiniMax
// specific is the optional GroupId query parameter
// (Extras["minimax.groupId"]). Per the project's "baseUrl must be
// origin-only" rule, CallTarget.BaseURL is required and the /v1 prefix
// is appended in Transport.BuildURL.
package minimax

import (
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
)

// NewSpec returns the MiniMax AdapterSpec.
func NewSpec(log *slog.Logger) provcore.AdapterSpec {
	if log == nil {
		log = slog.Default()
	}
	return provcore.AdapterSpec{
		Format:          provcore.FormatMiniMax,
		Transport:       NewTransport(log),
		SchemaCodec:     openai.IdentityCodec(),
		StreamDecoder:   openai.NewStreamDecoder(log),
		ErrorNormalizer: openai.ErrorNormalizerInstance(),
	}
}
