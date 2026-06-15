package generic

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// Normalize implements normalize.Normalizer. The generic-jsonpath
// adapter is configured per-domain via JSON-path extractors and may
// face any known wire format, so it offers the body to all three
// shared full-fidelity codecs and keeps the highest-confidence claim —
// the request shapes of OpenAI Chat and Anthropic Messages overlap
// (both carry model + messages[]), so a fixed first-claim order would
// mis-attribute one family's bodies to the other; the codecs' own
// field-shape confidence is the discriminator. When no codec claims,
// the chatgpt-web consumer pattern spec is the last probe. The winning
// payload's DetectedSpec is re-stamped with this adapter's ID so audit
// rows keep per-host provenance.
func (a *Adapter) Normalize(ctx context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	codecOrder := []normalize.Normalizer{
		codecs.SharedOpenAIChat(),
		codecs.SharedAnthropicMessages(),
		codecs.SharedGeminiGenerate(),
	}
	var best normalize.NormalizedPayload
	claimed := false
	for _, c := range codecOrder {
		p, err := c.Normalize(ctx, raw, meta)
		if err != nil {
			continue
		}
		if !claimed || p.Confidence > best.Confidence {
			best = p
			claimed = true
		}
	}
	if claimed {
		best.DetectedSpec = "generic-jsonpath"
		return best, nil
	}
	return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
		AdapterID:     "generic-jsonpath",
		ReqSpecIDs:    []string{"chatgpt-web"},
		RespSpecIDs:   []string{"chatgpt-web"},
		MinConfidence: 0.6,
	})
}
