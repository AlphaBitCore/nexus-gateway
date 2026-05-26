package anthropic

import (
	"context"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// Normalize implements normalize.Normalizer for the Anthropic Messages API
// (/v1/messages). Routes through extract.NormalizeForAdapter so adapter
// callers (compliance-proxy / agent) share one entry point with the rest of
// the framework; the dedicated AnthropicMessagesNormalizer remains the
// authoritative parser inside the AI gateway audit path.
func (a *Adapter) Normalize(_ context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID:     "anthropic",
		ReqSpecIDs:    []string{"anthropic-messages", "anthropic-completions-legacy"},
		RespSpecIDs:   []string{"anthropic-messages-nonstream", "anthropic-messages-sse"},
		MinConfidence: 0.5,
	})
}
