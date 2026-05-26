package claudeweb

import (
	"context"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// Normalize implements normalize.Normalizer. claude.ai web traffic uses the
// Anthropic Messages wire shape with browser-side extensions; the pattern
// probe scores against the anthropic-messages spec with confidence 0.5.
func (a *Adapter) Normalize(_ context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID: adapterID,
		// claude-web spec recognises the consumer-surface single-prompt
		// shape (top-level `prompt` + `parent_message_uuid` etc.).
		// anthropic-messages is kept as a fallback in case Anthropic ever
		// migrates the web client to a messages[] body shape.
		ReqSpecIDs:    []string{"claude-web", "anthropic-messages"},
		RespSpecIDs:   []string{"anthropic-messages-nonstream", "anthropic-messages-sse"},
		MinConfidence: 0.5,
	})
}
