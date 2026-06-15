package m365copilotweb

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Normalize implements normalize.Normalizer. This host speaks the
// OpenAI Chat wire shape, so decoding delegates to the shared
// full-fidelity OpenAI Chat codec; only DetectedSpec is re-stamped
// with this adapter's ID so audit rows keep per-host provenance.
// Decode failures propagate so the Coordinator falls through to
// Tier 2 / Tier 3.
func (a *Adapter) Normalize(ctx context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	p, err := codecs.SharedOpenAIChat().Normalize(ctx, raw, meta)
	if err != nil {
		return p, err
	}
	p.DetectedSpec = a.ID()
	return p, nil
}
