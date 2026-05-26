package bedrock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// embeddingEncodeRequest dispatches to the Titan or Cohere embed codec based
// on the modelID prefix from CallTarget.ProviderModelID.
//
// Dispatch rules (per observed Bedrock model IDs):
//   - "amazon.titan-embed-*" → encodeTitanEmbedRequest
//   - "cohere.embed-*"       → encodeCohereEmbedRequest
//
// Any other modelID returns a 400 ProviderError; callers must configure
// ProviderModelID correctly when routing to Bedrock embedding models.
func embeddingEncodeRequest(canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	modelID := target.ProviderModelID
	if modelID == "" {
		// Fall back to model from canonical body for identification only.
		modelID = gjson.GetBytes(canonicalBody, "model").Str
	}
	switch {
	case isTitanEmbedModel(modelID):
		return encodeTitanEmbedRequest(canonicalBody, target)
	case isCohereEmbedModel(modelID):
		return encodeCohereEmbedRequest(canonicalBody, target)
	default:
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: fmt.Sprintf("bedrock: unsupported embedding model %q — only amazon.titan-embed-* and cohere.embed-* are supported", modelID),
		}
	}
}

// embeddingDecodeResponse dispatches to the Titan or Cohere embed response
// decoder based on ProviderModelID.
func embeddingDecodeResponse(nativeBody []byte, modelID string) (provcore.DecodeResult, error) {
	switch {
	case isTitanEmbedModel(modelID):
		return decodeTitanEmbedResponse(nativeBody, modelID)
	case isCohereEmbedModel(modelID):
		return decodeCohereEmbedResponse(nativeBody, modelID)
	default:
		// Unknown model — attempt a generic parse of the canonical-like shape
		// returned by some Bedrock models.
		return decodeGenericBedrockEmbedResponse(nativeBody, modelID)
	}
}

// decodeBedrockEmbedResponseByShape dispatches to the appropriate embed
// response decoder by inspecting the response body shape rather than the
// modelID. This is used from codec.DecodeResponse, which does not receive
// CallTarget (and therefore does not have access to ProviderModelID).
//
// Dispatch logic:
//   - "embedding" key (flat float array)  → Titan response shape
//   - "embeddings" key (array of arrays)  → Cohere-on-Bedrock response shape
//   - fallback                            → generic decoder
func decodeBedrockEmbedResponseByShape(nativeBody []byte) (provcore.DecodeResult, error) {
	if !gjson.ValidBytes(nativeBody) {
		return provcore.DecodeResult{}, fmt.Errorf("bedrock-embed: invalid JSON response body")
	}
	if gjson.GetBytes(nativeBody, "embedding").IsArray() {
		// Titan response: {embedding:[...], inputTextTokenCount:N}
		return decodeTitanEmbedResponse(nativeBody, "")
	}
	if gjson.GetBytes(nativeBody, "embeddings").IsArray() {
		// Cohere-on-Bedrock response: {embeddings:[[...]], id, response_type, texts}
		return decodeCohereEmbedResponse(nativeBody, "")
	}
	// Fallback: attempt generic shape extraction.
	return decodeGenericBedrockEmbedResponse(nativeBody, "")
}

// decodeGenericBedrockEmbedResponse is a best-effort fallback for Bedrock
// embedding models whose response shape is not specifically handled. It tries
// to extract a single embedding vector from top-level "embedding" or
// "embeddings" fields.
func decodeGenericBedrockEmbedResponse(nativeBody []byte, modelID string) (provcore.DecodeResult, error) {
	if !gjson.ValidBytes(nativeBody) {
		return provcore.DecodeResult{}, fmt.Errorf("bedrock-embed: invalid JSON response body")
	}

	// Try "embedding" (Titan-style) then "embeddings" (Cohere-style).
	var row []float64
	if emb := gjson.GetBytes(nativeBody, "embedding"); emb.IsArray() {
		row = make([]float64, 0, 256)
		emb.ForEach(func(_, n gjson.Result) bool {
			row = append(row, n.Float())
			return true
		})
	} else if embs := gjson.GetBytes(nativeBody, "embeddings"); embs.IsArray() {
		arr := embs.Array()
		if len(arr) > 0 && arr[0].IsArray() {
			row = make([]float64, 0, 256)
			arr[0].ForEach(func(_, n gjson.Result) bool {
				row = append(row, n.Float())
				return true
			})
		}
	}

	canonical := map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"object": "embedding", "embedding": row, "index": 0},
		},
		"model": modelID,
		"usage": map[string]any{"prompt_tokens": 0, "total_tokens": 0},
	}
	canonicalBytes, err := json.Marshal(canonical)
	if err != nil {
		return provcore.DecodeResult{}, fmt.Errorf("bedrock-embed: marshal canonical: %w", err)
	}
	return provcore.DecodeResult{CanonicalBody: canonicalBytes}, nil
}

// EmbedRequestToCanonical translates a Bedrock /model/<modelId>/invoke
// embedding request body into canonical OpenAI embedding shape. The
// modelID is used to dispatch to the correct codec (Titan vs Cohere).
//
// This function is exposed for cross-format embedding routing.
func EmbedRequestToCanonical(body []byte, modelID string) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "bedrock embed: invalid JSON request body",
		}
	}
	var (
		canonical []byte
		err       error
	)
	switch {
	case isTitanEmbedModel(modelID):
		canonical, err = titanRequestToCanonical(body, modelID)
	case isCohereEmbedModel(modelID):
		canonical, err = cohereRequestToCanonical(body, modelID)
	default:
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: fmt.Sprintf("bedrock embed: unsupported model %q for canonical conversion", modelID),
		}
	}
	return canonical, err
}

// titanRequestToCanonical converts a Titan Embeddings wire request body
// {inputText, dimensions?, normalize?, embeddingTypes?} into canonical shape.
func titanRequestToCanonical(body []byte, modelID string) ([]byte, error) {
	inputText := gjson.GetBytes(body, "inputText").Str
	if inputText == "" {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "bedrock-titan embed: missing 'inputText' field",
		}
	}

	canonical := []byte(`{}`)
	canonical, _ = sjson.SetBytes(canonical, "model", modelID)
	canonical, _ = sjson.SetBytes(canonical, "input", inputText)

	// Forward dimensions as canonical dimensions.
	if dim := gjson.GetBytes(body, "dimensions"); dim.Exists() && dim.Type == gjson.Number {
		canonical, _ = sjson.SetBytes(canonical, "dimensions", dim.Int())
	}

	// Titan-specific extensions → nexus.ext.bedrock.*
	var stampErr error
	if norm := gjson.GetBytes(body, "normalize"); norm.Exists() {
		canonical, stampErr = canonicalext.Set(canonical, "bedrock", "titan_normalize", norm.Bool())
		if stampErr != nil {
			return nil, fmt.Errorf("bedrock-titan embed: stamp normalize: %w", stampErr)
		}
	}
	if et := gjson.GetBytes(body, "embeddingTypes"); et.IsArray() {
		types := make([]string, 0, 2)
		et.ForEach(func(_, v gjson.Result) bool {
			if v.Type == gjson.String {
				types = append(types, v.Str)
			}
			return true
		})
		if len(types) > 0 {
			canonical, stampErr = canonicalext.Set(canonical, "bedrock", "titan_embedding_types", types)
			if stampErr != nil {
				return nil, fmt.Errorf("bedrock-titan embed: stamp embeddingTypes: %w", stampErr)
			}
		}
	}
	return canonical, nil
}

// cohereRequestToCanonical converts a Cohere Embed on Bedrock wire request
// {texts, input_type, truncate?, embedding_types?} into canonical shape.
func cohereRequestToCanonical(body []byte, modelID string) ([]byte, error) {
	textsVal := gjson.GetBytes(body, "texts")
	if !textsVal.IsArray() || textsVal.Array() == nil {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "bedrock-cohere embed: missing 'texts' array",
		}
	}
	texts := make([]string, 0)
	var parseErr error
	textsVal.ForEach(func(_, v gjson.Result) bool {
		if v.Type != gjson.String {
			parseErr = &provcore.ProviderError{
				Status:  http.StatusBadRequest,
				Code:    provcore.CodeInvalidRequest,
				Message: "bedrock-cohere embed: non-string element in 'texts' array",
			}
			return false
		}
		texts = append(texts, v.Str)
		return true
	})
	if parseErr != nil {
		return nil, parseErr
	}
	if len(texts) == 0 {
		return nil, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "bedrock-cohere embed: 'texts' array is empty",
		}
	}

	canonical := []byte(`{}`)
	canonical, _ = sjson.SetBytes(canonical, "model", modelID)
	canonical, _ = sjson.SetBytes(canonical, "input", texts)

	// Cohere-on-Bedrock extensions → nexus.ext.bedrock.*
	var stampErr error
	if it := gjson.GetBytes(body, "input_type"); it.Type == gjson.String && it.Str != "" {
		canonical, stampErr = canonicalext.Set(canonical, "bedrock", "cohere_input_type", it.Str)
		if stampErr != nil {
			return nil, fmt.Errorf("bedrock-cohere embed: stamp input_type: %w", stampErr)
		}
	}
	if tr := gjson.GetBytes(body, "truncate"); tr.Type == gjson.String && tr.Str != "" {
		canonical, stampErr = canonicalext.Set(canonical, "bedrock", "cohere_truncate", tr.Str)
		if stampErr != nil {
			return nil, fmt.Errorf("bedrock-cohere embed: stamp truncate: %w", stampErr)
		}
	}
	if et := gjson.GetBytes(body, "embedding_types"); et.IsArray() {
		types := make([]string, 0, 2)
		et.ForEach(func(_, v gjson.Result) bool {
			if v.Type == gjson.String {
				types = append(types, v.Str)
			}
			return true
		})
		if len(types) > 0 {
			canonical, stampErr = canonicalext.Set(canonical, "bedrock", "cohere_embedding_types", types)
			if stampErr != nil {
				return nil, fmt.Errorf("bedrock-cohere embed: stamp embedding_types: %w", stampErr)
			}
		}
	}
	return canonical, nil
}

// CanonicalToBedrockEmbedResponse converts a canonical OpenAI-shape embedding
// response into the Bedrock wire shape for the given model. Used by
// cross-format routing when translating between canonical and Bedrock wire.
func CanonicalToBedrockEmbedResponse(canonical []byte, modelID string) ([]byte, error) {
	if !gjson.ValidBytes(canonical) {
		return nil, fmt.Errorf("bedrock embed response: invalid canonical body")
	}
	switch {
	case isTitanEmbedModel(modelID):
		return canonicalToTitanEmbedResponse(canonical)
	case isCohereEmbedModel(modelID):
		return canonicalToCohereEmbedResponse(canonical, modelID)
	default:
		return canonicalToTitanEmbedResponse(canonical)
	}
}

// canonicalToTitanEmbedResponse converts canonical → Titan wire response.
func canonicalToTitanEmbedResponse(canonical []byte) ([]byte, error) {
	data := gjson.GetBytes(canonical, "data")
	if !data.IsArray() || len(data.Array()) == 0 {
		return nil, fmt.Errorf("bedrock-titan embed response: canonical body missing data[]")
	}
	embVal := data.Array()[0].Get("embedding")
	row := make([]float64, 0)
	if embVal.IsArray() {
		embVal.ForEach(func(_, n gjson.Result) bool {
			row = append(row, n.Float())
			return true
		})
	}
	tokenCount := gjson.GetBytes(canonical, "usage.prompt_tokens").Int()
	if tokenCount == 0 {
		tokenCount = gjson.GetBytes(canonical, "usage.total_tokens").Int()
	}
	out := map[string]any{
		"embedding":            row,
		"inputTextTokenCount":  tokenCount,
	}
	return json.Marshal(out)
}

// canonicalToCohereEmbedResponse converts canonical → Cohere Embed on Bedrock
// wire response.
func canonicalToCohereEmbedResponse(canonical []byte, modelID string) ([]byte, error) {
	data := gjson.GetBytes(canonical, "data")
	if !data.IsArray() {
		return nil, fmt.Errorf("bedrock-cohere embed response: canonical body missing data[]")
	}
	embeddings := make([][]float64, 0)
	data.ForEach(func(_, item gjson.Result) bool {
		embVal := item.Get("embedding")
		row := make([]float64, 0)
		if embVal.IsArray() {
			embVal.ForEach(func(_, n gjson.Result) bool {
				row = append(row, n.Float())
				return true
			})
		}
		embeddings = append(embeddings, row)
		return true
	})
	// Build a model-derived response_type and id.
	respType := "embeddings_floats"
	if strings.Contains(strings.ToLower(modelID), "int8") {
		respType = "embeddings_int8"
	}
	out := map[string]any{
		"id":            "bedrock-" + modelID,
		"embeddings":    embeddings,
		"response_type": respType,
	}
	return json.Marshal(out)
}
