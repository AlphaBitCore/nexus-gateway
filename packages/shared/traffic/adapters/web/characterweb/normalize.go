package characterweb

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// Normalize implements normalize.Normalizer for character.ai. The host
// speaks OpenAI-Chat-compatible JSON on its chat endpoints, so both
// directions delegate to the shared OpenAI Chat codec first
// (DetectedSpec re-stamped for per-host provenance). Roleplay requests
// additionally ship a flat-prompt body (single `prompt` field plus
// character context metadata) that the codec rejects — bodies without
// a messages[] array fall back to the openai-completions-legacy
// pattern spec, the one surviving flat-prompt decoder.
func (a *Adapter) Normalize(ctx context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	p, err := codecs.SharedOpenAIChat().Normalize(ctx, raw, meta)
	if err == nil {
		p.DetectedSpec = "character-web"
		return p, nil
	}
	if meta.Direction != normalize.DirectionRequest {
		return p, err
	}
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID:     "character-web",
		ReqSpecIDs:    []string{"openai-completions-legacy"},
		MinConfidence: 0.5,
	})
}
