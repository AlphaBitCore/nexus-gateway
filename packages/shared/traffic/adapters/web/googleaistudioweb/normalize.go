package googleaistudioweb

import (
	"context"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// Normalize implements normalize.Normalizer. Delegates to
// extract.NormalizeForAdapter with the gemini-generate spec.
func (a *Adapter) Normalize(_ context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID:     "google-ai-studio-web",
		ReqSpecIDs:    []string{"gemini-generate"},
		RespSpecIDs:   []string{"gemini-generate-nonstream", "gemini-generate-sse"},
		MinConfidence: 0.5,
	})
}
