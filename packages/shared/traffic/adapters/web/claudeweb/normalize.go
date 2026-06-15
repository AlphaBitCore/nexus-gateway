package claudeweb

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// Normalize implements normalize.Normalizer for claude.ai (consumer web).
//
// Requests use the consumer-surface single-prompt shape (top-level
// `prompt` + `parent_message_uuid` etc. — server-side conversation
// history, never a messages[] array), recognised by the claude-web
// pattern spec. Responses are standard Anthropic Messages SSE / JSON,
// so the response direction delegates to the shared full-fidelity
// Anthropic Messages codec with DetectedSpec re-stamped for per-host
// provenance.
func (a *Adapter) Normalize(ctx context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	if meta.Direction == normalize.DirectionResponse {
		p, err := codecs.SharedAnthropicMessages().Normalize(ctx, raw, meta)
		if err != nil {
			return p, err
		}
		p.DetectedSpec = adapterID
		return p, nil
	}
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID:     adapterID,
		ReqSpecIDs:    []string{"claude-web"},
		MinConfidence: 0.5,
	})
}
