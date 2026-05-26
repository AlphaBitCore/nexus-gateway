package provbuiltins

import (
	"log/slog"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/azure"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/bedrock"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/cohere"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/deepseek"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/fireworks"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/glm"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/groq"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/huggingface"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/minimax"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/mistral"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/moonshot"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/voyage"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/perplexity"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/replicate"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/together"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/vertex"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/compat/xai"
)

// SchemaCodecs returns a fresh map of wire format → SchemaCodec for every
// built-in provider. Used by the canonical ingress/egress bridge so codec
// instances stay aligned with [Register].
func SchemaCodecs(log *slog.Logger) map[provcore.Format]provcore.SchemaCodec {
	if log == nil {
		log = slog.Default()
	}
	return map[provcore.Format]provcore.SchemaCodec{
		provcore.FormatOpenAI:      openai.NewSpec(log).SchemaCodec,
		provcore.FormatDeepSeek:    deepseek.NewSpec(log).SchemaCodec,
		provcore.FormatGLM:         glm.NewSpec(log).SchemaCodec,
		provcore.FormatAzureOpenAI: azure.NewSpec(log).SchemaCodec,
		provcore.FormatAnthropic:   anthropic.NewSpec(log).SchemaCodec,
		provcore.FormatGemini:      gemini.NewSpec(log).SchemaCodec,
		provcore.FormatMiniMax:     minimax.NewSpec(log).SchemaCodec,
		provcore.FormatBedrock:     bedrock.NewSpec(log).SchemaCodec,
		provcore.FormatVertex:      vertex.NewSpec(log).SchemaCodec,
		provcore.FormatCohere:      cohere.NewSpec(log).SchemaCodec,
		provcore.FormatHuggingFace: huggingface.NewSpec(log).SchemaCodec,
		provcore.FormatReplicate:   replicate.NewSpec(log).SchemaCodec,
		provcore.FormatMistral:     mistral.NewSpec(log).SchemaCodec,
		provcore.FormatXai:         xai.NewSpec(log).SchemaCodec,
		provcore.FormatGroq:        groq.NewSpec(log).SchemaCodec,
		provcore.FormatPerplexity:  perplexity.NewSpec(log).SchemaCodec,
		provcore.FormatTogether:    together.NewSpec(log).SchemaCodec,
		provcore.FormatFireworks:   fireworks.NewSpec(log).SchemaCodec,
		provcore.FormatMoonshot:    moonshot.NewSpec(log).SchemaCodec,
		provcore.FormatVoyage:      voyage.NewSpec(log).SchemaCodec,
	}
}
