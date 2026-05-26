// Package codec — OpenAI embedding request encoding.
//
// Architecture references:
//   - docs/dev/architecture/provider-adapter-architecture.md §3a Rules 1-7
//   - docs/dev/architecture/endpoint-typology-architecture.md §2
//
// Per-model wire rules are listed with empirical 400 citations per Rule 7.
package codec

import (
	"fmt"
	"net/http"
	"regexp"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ada002Regex matches the text-embedding-ada-002 model family.
// ada-002 rejects "dimensions" and "encoding_format" — observed 400
// "Unrecognized request argument supplied: dimensions" (OpenAI API,
// reproduced against api.openai.com with model=text-embedding-ada-002).
var ada002Regex = regexp.MustCompile(`^text-embedding-ada-002`)

// encodeOpenAIEmbeddingRequest applies per-model wire rules to a canonical
// OpenAI-shaped embedding request body. The canonical OpenAI shape IS the
// OpenAI wire shape for embeddings (Rule 1) so this is mostly a no-op
// except for ada-002 which rejects parameters newer models accept.
func encodeOpenAIEmbeddingRequest(canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
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

	// Resolve the model ID: prefer target.ProviderModelID (populated by
	// the routing resolver) then fall back to the body's "model" field.
	modelID := target.ProviderModelID
	if modelID == "" {
		modelID = gjson.GetBytes(canonicalBody, "model").Str
	}

	var rewrites []string
	body := canonicalBody

	if ada002Regex.MatchString(modelID) {
		// ada-002 rejects dimensions and encoding_format — observed 400
		// "Unrecognized request argument supplied: dimensions" (OpenAI API).
		// Strip both fields from the wire body. sjson.DeleteBytes
		// only errors on malformed path strings; the literal keys here are
		// guaranteed valid so the error is structurally unreachable.
		if gjson.GetBytes(body, "dimensions").Exists() {
			body, _ = sjson.DeleteBytes(body, "dimensions")
			rewrites = append(rewrites, "dimensions→removed (ada-002: unsupported field)")
		}
		if gjson.GetBytes(body, "encoding_format").Exists() {
			body, _ = sjson.DeleteBytes(body, "encoding_format")
			rewrites = append(rewrites, "encoding_format→removed (ada-002: unsupported field)")
		}
	} else {
		// text-embedding-3-* and other models: pass through. Apply a
		// codec safety-net per SDD §T1.2/T5.5: if "dimensions" is
		// present it must be a positive integer (the routing pre-filter
		// already validated it against supported_dimensions; this check
		// catches any malformed pass-through that bypassed pre-filter).
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
