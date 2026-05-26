package generic

import (
	"context"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// Normalize implements normalize.Normalizer. The generic-jsonpath adapter is
// configured per-domain via JSON-path extractors and may match any known
// spec depending on which provider the operator targeted. Tries every spec
// and claims the best-scoring match.
func (a *Adapter) Normalize(_ context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID:     "generic-jsonpath",
		ReqSpecIDs:    []string{"openai-chat", "anthropic-messages", "gemini-generate", "chatgpt-web"},
		RespSpecIDs:   []string{"openai-chat-nonstream", "openai-chat-sse", "anthropic-messages-nonstream", "anthropic-messages-sse", "gemini-generate-nonstream", "gemini-generate-sse"},
		MinConfidence: 0.6,
	})
}
