package characterweb

import (
	"context"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// Normalize implements normalize.Normalizer. character.ai uses a
// roleplay-shaped request with a single prompt field plus character context
// metadata; falls back to legacy completion-style specs for single-prompt
// bodies. Low-confidence matches degrade to Tier 2 or Tier 3.
func (a *Adapter) Normalize(_ context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID:     "character-web",
		ReqSpecIDs:    []string{"anthropic-completions-legacy", "openai-completions-legacy", "openai-chat"},
		RespSpecIDs:   []string{"openai-chat-nonstream", "openai-chat-sse"},
		MinConfidence: 0.5,
	})
}
