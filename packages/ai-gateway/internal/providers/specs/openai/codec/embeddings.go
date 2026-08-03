// Package codec — OpenAI embedding request encoding (canonical door).
//
// Architecture references:
//   - docs/dev/architecture/provider-adapter-architecture.md §3a Rules 1-7
//   - docs/dev/architecture/endpoint-typology-architecture.md §2
//
// Per-model wire rules ride the contract (specs/openai/rewrites carries
// the rules with their empirical 400 citations per Rule 7); this file owns
// the canonical-door-only validation.
package codec

import (
	"fmt"
	"net/http"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// encodeEmbeddings applies the contract's per-model wire rules and the
// resolved-model stamp to a canonical OpenAI-shaped embedding request
// body. The canonical OpenAI shape IS the OpenAI wire shape for
// embeddings (Rule 1), so no translation happens; the door owes the same
// differential RewriteNative's embeddings arm owes, plus the
// canonical-door-only validation below (the native door forwards what it
// does not rewrite and lets the upstream validate its own wire).
func (c *identityCodec) encodeEmbeddings(canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	if len(canonicalBody) == 0 {
		return provcore.EncodeResult{ContentType: "application/json"}, nil
	}
	if !gjson.ValidBytes(canonicalBody) {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "openai embed: invalid canonical JSON body",
		}
	}

	// Resolve the model ID for rule gating: prefer target.ProviderModelID
	// (populated by the routing resolver) then fall back to the body's
	// "model" field. The stamp itself only ever writes the resolved
	// target id — a body-derived fallback must not overwrite anything.
	modelID := target.ProviderModelID
	if modelID == "" {
		modelID = gjson.GetBytes(canonicalBody, "model").Str
	}

	stamped, err := provcore.SurgicalModelStamp(canonicalBody, target.ProviderModelID)
	if err != nil {
		return provcore.EncodeResult{}, err
	}
	body, rewrites, ok := c.embeddings.applyBytes(stamped, modelID)
	if !ok {
		return c.mapDoorGated(canonicalBody, target, &c.embeddings, false, modelID)
	}

	if c.embeddings.dueMask(modelID) == 0 {
		// No per-model rule matched (text-embedding-3-* and other
		// models): pass through with a codec safety-net per SDD
		// §T1.2/T5.5 — if "dimensions" is present it must be a positive
		// integer (the routing pre-filter already validated it against
		// supported_dimensions; this check catches any malformed
		// pass-through that bypassed pre-filter). Canonical door only:
		// contract-external validation never runs on the native door.
		if dimVal := gjson.GetBytes(body, "dimensions"); dimVal.Exists() {
			if dimVal.Type != gjson.Number || dimVal.Int() <= 0 {
				return provcore.EncodeResult{}, &provcore.ProviderError{
					Status:  http.StatusBadRequest,
					Code:    provcore.CodeInvalidRequest,
					Message: fmt.Sprintf("openai embed: dimensions must be a positive integer, got %s", dimVal.Raw),
				}
			}
		}
	}

	return provcore.EncodeResult{
		Body:        body,
		ContentType: "application/json",
		Rewrites:    rewrites,
	}, nil
}
