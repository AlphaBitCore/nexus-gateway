// Package codec implements the GLM SchemaCodec.
//
// GLM (Zhipu AI) exposes an OpenAI-compatible surface:
//   - Chat completions: /api/paas/v4/chat/completions — identical wire shape
//   - Embeddings:       /api/paas/v4/embeddings      — OpenAI-compatible
//     request ({model, input}) and response ({object, data, usage}) BUT
//     GLM does NOT support integer token arrays as input. Token inputs
//     must be rejected with a 400 (Rule 3 per-wire quirk).
//
// For chat/completions the codec is identity (the OpenAI shape IS the GLM
// wire shape). For embeddings it adds the token-rejection per-wire rule.
package codec

import (
	"fmt"
	"net/http"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// GLMCodec is the SchemaCodec for GLM.
//
// Chat completions: identity — GLM wire shape == canonical OpenAI shape.
// Embeddings: OpenAI-compatible but token arrays are unsupported — any
// request whose "input" field is an integer array or array of integer arrays
// is rejected with a 400 per Rule 3 (per-model wire quirk).
// DecodeResponse: identity for all endpoints; Usage extraction is delegated
// to provcore.ExtractUsage (shared OpenAI normalizer) consistent with Rule 5.
type GLMCodec struct{}

// New returns the GLM SchemaCodec.
func New() provcore.SchemaCodec { return GLMCodec{} }

// EncodeRequest translates a canonical OpenAI-shaped body to the GLM wire body.
//
//   - chat_completions / responses / models / completions_legacy: identity pass-through.
//   - embeddings: pass-through EXCEPT token-array inputs, which are rejected.
//     GLM /api/paas/v4/embeddings does not support integer token inputs —
//     the endpoint returns 400 "invalid_request_error" for such inputs
//     (observed against open.bigmodel.cn/api/paas/v4/embeddings, embedding-3).
//     Rule 3 (per-wire quirk): token rejection lives here
//     in the adapter that talks to the GLM wire, not in the generic dispatcher.
func (GLMCodec) EncodeRequest(endpoint typology.WireShape, canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	switch endpoint {
	case typology.WireShapeOpenAIChat, typology.WireShapeOpenAIResponses,
		typology.WireShapeNone, typology.WireShapeOpenAICompletionsLegacy:
		return provcore.EncodeResult{Body: canonicalBody, ContentType: "application/json"}, nil

	case typology.WireShapeOpenAIEmbeddings:
		return encodeGLMEmbeddingRequest(canonicalBody, target)
	}
	return provcore.EncodeResult{}, fmt.Errorf("glm codec: unsupported endpoint %q", endpoint)
}

// DecodeResponse is identity for all endpoints. GLM embedding and chat
// responses match the canonical OpenAI shape. Usage is extracted via the
// shared Tier-1 normalizer for consistent token accounting across all services.
func (GLMCodec) DecodeResponse(_ typology.WireShape, nativeBody []byte, _ string) (provcore.DecodeResult, error) {
	return provcore.DecodeResult{
		CanonicalBody: nativeBody,
		Usage:         provcore.ExtractUsage(nativeBody, provcore.FormatGLM),
	}, nil
}

// encodeGLMEmbeddingRequest applies the GLM-specific token-rejection rule.
//
// GLM accepts:
//   - string       → bare JSON string in "input"
//   - []string     → JSON array of strings in "input"
//
// GLM rejects:
//   - []int        → integer token array — observed 400 "invalid_request_error:
//     token input not supported" (open.bigmodel.cn/api/paas/v4/embeddings
//     embedding-3). Rule 7: documented here at the adapter.
//   - [][]int      → batch token arrays — same 400 as above.
//
// For valid inputs the body is forwarded verbatim (GLM wire == canonical shape).
func encodeGLMEmbeddingRequest(canonicalBody []byte, _ provcore.CallTarget) (provcore.EncodeResult, error) {
	if len(canonicalBody) == 0 {
		return provcore.EncodeResult{ContentType: "application/json"}, nil
	}
	if !gjson.ValidBytes(canonicalBody) {
		return provcore.EncodeResult{}, &provcore.ProviderError{
			Status:  http.StatusBadRequest,
			Code:    provcore.CodeInvalidRequest,
			Message: "glm embed: invalid canonical JSON body",
		}
	}

	inputVal := gjson.GetBytes(canonicalBody, "input")
	if !inputVal.Exists() {
		// Missing input: pass through and let the upstream return its own 400.
		// We do not silently inject a default because Rule 3 restricts us to
		// per-wire quirks only — input validation is the caller's responsibility.
		return provcore.EncodeResult{Body: canonicalBody, ContentType: "application/json"}, nil
	}

	if inputVal.IsArray() {
		arr := inputVal.Array()
		if len(arr) > 0 {
			first := arr[0]
			switch {
			case first.Type == gjson.Number:
				// Array of integers → single token sequence. GLM does not
				// support integer token inputs — observed 400
				// "invalid_request_error: token input not supported"
				// (open.bigmodel.cn/api/paas/v4/embeddings, embedding-3).
				// Rule 7: error message cited at the adapter.
				return provcore.EncodeResult{}, &provcore.ProviderError{
					Status:  http.StatusBadRequest,
					Code:    provcore.CodeInvalidRequest,
					Message: "glm embed: token_array_unsupported_by_glm — GLM /api/paas/v4/embeddings does not accept integer token inputs; use string inputs instead",
				}
			case first.IsArray():
				// Array of integer arrays → batch token input. Same 400 as above.
				return provcore.EncodeResult{}, &provcore.ProviderError{
					Status:  http.StatusBadRequest,
					Code:    provcore.CodeInvalidRequest,
					Message: "glm embed: token_array_unsupported_by_glm — GLM /api/paas/v4/embeddings does not accept token array inputs",
				}
			}
			// Otherwise: array of strings → valid, fall through.
		}
	}
	// String or array-of-strings: pass through verbatim.
	return provcore.EncodeResult{Body: canonicalBody, ContentType: "application/json"}, nil
}
