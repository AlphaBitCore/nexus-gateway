package anthropicconsoleweb

import (
	"context"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// Normalize implements normalize.Normalizer. Delegates to
// extract.NormalizeForAdapter with the anthropic-messages spec.
func (a *Adapter) Normalize(_ context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID:     "anthropic-console-web",
		ReqSpecIDs:    []string{"anthropic-messages"},
		RespSpecIDs:   []string{"anthropic-messages-nonstream", "anthropic-messages-sse"},
		MinConfidence: 0.5,
	})
}
