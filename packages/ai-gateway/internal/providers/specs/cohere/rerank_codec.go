package cohere

import (
	"fmt"

	"github.com/goccy/go-json"
	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// rerank_codec.go — the Cohere /v2/rerank leg of the codec. The canonical
// rerank shape IS the Cohere shape ({model, query, documents, top_n,
// return_documents} → {id, results:[{index, relevance_score, document}],
// meta:{billed_units:{search_units}}}), so both directions are near-identity:
//
//   - encode: pass the canonical body through, injecting `model` from the
//     resolved target when the caller omitted it. (On the common Cohere→Cohere
//     path the native leg runs RewriteNative, which stamps the resolved model
//     over any client-facing alias; this encode runs only if a future
//     non-Cohere rerank ingress is added and canonicalizes to the Cohere shape.)
//   - decode: identity. The Cohere response already matches the canonical
//     rerank contract, and its `meta.billed_units.search_units` must survive
//     verbatim — the cost stamper reads it off the canonical body.

// encodeCohereRerank passes a canonical (Cohere-shaped) rerank body through to
// the Cohere /v2/rerank wire, injecting the resolved model when absent.
func encodeCohereRerank(canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	if len(canonicalBody) == 0 {
		return provcore.EncodeResult{}, fmt.Errorf("cohere: empty rerank body")
	}
	if !gjson.ValidBytes(canonicalBody) {
		return provcore.EncodeResult{}, fmt.Errorf("cohere: invalid rerank body")
	}
	if !gjson.GetBytes(canonicalBody, "model").Exists() && target.ProviderModelID != "" {
		var obj map[string]any
		if err := json.Unmarshal(canonicalBody, &obj); err != nil {
			return provcore.EncodeResult{}, err
		}
		obj["model"] = target.ProviderModelID
		body, err := json.Marshal(obj)
		if err != nil {
			return provcore.EncodeResult{}, err
		}
		return provcore.EncodeResult{Body: body, ContentType: "application/json"}, nil
	}
	return provcore.EncodeResult{Body: canonicalBody, ContentType: "application/json"}, nil
}

// decodeCohereRerankResponse is identity: the Cohere rerank response already
// matches the canonical rerank contract (results[] sorted by relevance_score,
// meta.billed_units.search_units). The search_units field is left in the body
// for the cost stamper (stampModalityUnits) to read; rerank reports no token
// usage, so DecodeResult.Usage stays empty.
func decodeCohereRerankResponse(nativeBody []byte) (provcore.DecodeResult, error) {
	if len(nativeBody) == 0 {
		return provcore.DecodeResult{CanonicalBody: nativeBody}, nil
	}
	if !gjson.ValidBytes(nativeBody) {
		return provcore.DecodeResult{}, fmt.Errorf("cohere: invalid rerank response body")
	}
	return provcore.DecodeResult{CanonicalBody: nativeBody}, nil
}
