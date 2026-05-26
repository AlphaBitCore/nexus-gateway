package together

import (
	"context"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// Normalize implements normalize.Normalizer. Delegates to extract.NormalizeForAdapter
// with the openai-chat spec; low-confidence matches fall through to Tier 2.
func (a *Adapter) Normalize(_ context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID:     "together",
		ReqSpecIDs:    []string{"openai-chat"},
		RespSpecIDs:   []string{"openai-chat-nonstream", "openai-chat-sse"},
		MinConfidence: 0.5,
	})
}
