package voyage

import (
	"fmt"
	"net/http"

	"github.com/goccy/go-json"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// rerank_codec.go — the Voyage AI /v1/rerank leg. The canonical rerank shape
// is Cohere's (query + documents + top_n), so this adapter owns the
// canonical→Voyage translation and back.
//
// Voyage rerank wire request:  {model, query, documents:[]string, top_k?,
//                               return_documents?, truncation?}
// Voyage rerank wire response: {object:"list",
//                               data:[{index, relevance_score, document?}],
//                               model, usage:{total_tokens:N}}
//
// The one wire quirk vs the Cohere-shaped canonical is the result cap field
// name: canonical `top_n` → Voyage `top_k` (observed field name in the Voyage
// rerank API). Documents come back as a bare string; the canonical (Cohere)
// contract wraps them as {text:...}.

// encodeVoyageRerank translates a canonical (Cohere-shaped) rerank body to the
// Voyage /v1/rerank wire.
func encodeVoyageRerank(canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	if len(canonicalBody) == 0 || !gjson.ValidBytes(canonicalBody) {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "voyage: invalid canonical rerank body",
		}
	}
	model := target.ProviderModelID
	if model == "" {
		model = gjson.GetBytes(canonicalBody, "model").Str
	}
	if model == "" {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "voyage: missing model — set via CallTarget.ProviderModelID or canonical body",
		}
	}
	query := gjson.GetBytes(canonicalBody, "query")
	if query.Type != gjson.String || query.Str == "" {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "voyage: canonical 'query' must be a non-empty string",
		}
	}
	docs := gjson.GetBytes(canonicalBody, "documents")
	if !docs.IsArray() {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "voyage: canonical 'documents' must be an array of strings",
		}
	}
	documents := make([]string, 0, len(docs.Array()))
	for _, d := range docs.Array() {
		if d.Type != gjson.String {
			return provcore.EncodeResult{}, &provcore.ProviderError{
				Status:  http.StatusBadRequest,
				Code:    provcore.CodeInvalidRequest,
				Message: "voyage: every 'documents' entry must be a string",
			}
		}
		documents = append(documents, d.Str)
	}

	wire := []byte(`{}`)
	wire, _ = sjson.SetBytes(wire, "model", model)
	wire, _ = sjson.SetBytes(wire, "query", query.Str)
	wire, _ = sjson.SetBytes(wire, "documents", documents)
	// Canonical `top_n` → Voyage `top_k` (observed Voyage rerank field name).
	if n := gjson.GetBytes(canonicalBody, "top_n"); n.Exists() && n.Type == gjson.Number {
		wire, _ = sjson.SetBytes(wire, "top_k", n.Int())
	}
	if rd := gjson.GetBytes(canonicalBody, "return_documents"); rd.Type == gjson.True || rd.Type == gjson.False {
		wire, _ = sjson.SetBytes(wire, "return_documents", rd.Bool())
	}
	return provcore.EncodeResult{Body: wire, ContentType: "application/json"}, nil
}

// decodeVoyageRerankResponse converts a Voyage rerank response into the
// canonical (Cohere-shaped) rerank response. Voyage reports billing as
// usage.total_tokens; it is stamped as DecodeResult.Usage.PromptTokens so the
// token cost path prices it (rerank has no completion tokens), and mirrored
// into meta.billed_units.total_tokens for the client contract.
func decodeVoyageRerankResponse(nativeBody []byte) (provcore.DecodeResult, error) {
	if len(nativeBody) == 0 {
		return provcore.DecodeResult{CanonicalBody: nativeBody}, nil
	}
	if !gjson.ValidBytes(nativeBody) {
		return provcore.DecodeResult{}, fmt.Errorf("voyage rerank response: invalid JSON body")
	}

	results := make([]map[string]any, 0)
	gjson.GetBytes(nativeBody, "data").ForEach(func(_, item gjson.Result) bool {
		r := map[string]any{
			"index":           item.Get("index").Int(),
			"relevance_score": item.Get("relevance_score").Float(),
		}
		if doc := item.Get("document"); doc.Exists() && doc.Type == gjson.String {
			// Voyage returns a bare string; the canonical (Cohere) contract
			// wraps the document as {text:...}.
			r["document"] = map[string]any{"text": doc.Str}
		}
		results = append(results, r)
		return true
	})

	var totalTokens int
	if tt := gjson.GetBytes(nativeBody, "usage.total_tokens"); tt.Exists() {
		totalTokens = int(tt.Int())
	}

	canonical := map[string]any{
		"model":   gjson.GetBytes(nativeBody, "model").Str,
		"results": results,
		"meta": map[string]any{
			"billed_units": map[string]any{"total_tokens": totalTokens},
		},
	}
	body, err := json.Marshal(canonical)
	if err != nil {
		return provcore.DecodeResult{}, fmt.Errorf("voyage rerank response: marshal canonical: %w", err)
	}

	var usage provcore.Usage
	if totalTokens > 0 {
		usage.PromptTokens = &totalTokens
		usage.TotalTokens = &totalTokens
	}
	return provcore.DecodeResult{CanonicalBody: body, Usage: usage}, nil
}
