package envelope

import (
	"encoding/json"
	"fmt"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// encodeErrorEnvelopeForIngress reshapes a normalised provider error
// into the envelope shape the client's ingress format expects.
//
// When the upstream's wire format matches the client's ingress format
// (e.g. OpenAI client → OpenAI provider, Anthropic client → Anthropic
// provider) we return the upstream raw body verbatim so native SDKs
// keep seeing the provider-specific fields they may parse beyond
// `error.message` (request_id, error.param, etc.).
//
// When ingress and upstream differ (the common case for our routing:
// OpenAI client → Anthropic upstream via the spec_anthropic codec) we
// build a fresh envelope in the ingress format using the canonical
// fields the [provcore.ErrorNormalizer] already extracted. This is
// the error-path twin of canonicalbridge.ResponseCanonicalToIngress
// which only handles the 2xx body — the 4xx body had no equivalent and
// a strict OpenAI SDK receiving an Anthropic-shape envelope would fail
// to deserialise the body and surface an "empty response" to the user
// (incident: traffic d914275a-0dae-4d13-a811-69e4d432c441).
//
// pe must be non-nil; callers gate on ExecutionResult.ProviderError.
// EncodeErrorEnvelopeForIngress reshapes a normalised provider error
// into the envelope shape the client's ingress format expects.
// Exported for use by ingress/proxy.
func EncodeErrorEnvelopeForIngress(ingress, upstream provcore.Format, pe *provcore.ProviderError) []byte {
	return encodeErrorEnvelopeForIngress(ingress, upstream, pe)
}

func encodeErrorEnvelopeForIngress(ingress, upstream provcore.Format, pe *provcore.ProviderError) []byte {
	if ingress == upstream || (ingress.IsOpenAIFamily() && upstream.IsOpenAIFamily()) {
		// Same-shape passthrough. Preserve upstream bytes verbatim so
		// native SDKs receive the full provider envelope.
		if len(pe.Raw) > 0 {
			return pe.Raw
		}
		// Synthetic error with no upstream body (e.g. auth failure
		// before dispatch) — fall through to the per-format encoder so
		// we emit *something* parseable.
	}

	switch ingress {
	case provcore.FormatAnthropic:
		return encodeAnthropicErrorEnvelope(pe)
	case provcore.FormatGemini, provcore.FormatVertex:
		return encodeGeminiErrorEnvelope(pe)
	case provcore.FormatOpenAIResponses:
		return encodeResponsesAPIErrorEnvelope(pe)
	default:
		// OpenAI-compat family and anything else default to OpenAI
		// shape, which is what /v1/chat/completions, /v1/embeddings,
		// and most clients calling the gateway expect.
		return encodeOpenAIErrorEnvelope(pe)
	}
}

// encodeOpenAIErrorEnvelope emits the OpenAI shape:
//
//	{"error": {"message": "...", "type": "...", "code": "...", "param": null}}
//
// Field names match the Python openai SDK's APIError construction so
// langchain / openai-node / spring-ai parse the message field cleanly.
func encodeOpenAIErrorEnvelope(pe *provcore.ProviderError) []byte {
	inner := map[string]any{
		"message": pe.Message,
		"type":    openaiErrorType(pe),
		"code":    pe.Code,
		"param":   nil,
	}
	body, _ := json.Marshal(map[string]any{"error": inner})
	return body
}

// encodeAnthropicErrorEnvelope emits the Anthropic shape:
//
//	{"type": "error", "error": {"type": "...", "message": "..."}}
//
// Matches the envelope that real api.anthropic.com returns so native
// Anthropic SDKs (anthropic-sdk-python, …) deserialise without changes.
func encodeAnthropicErrorEnvelope(pe *provcore.ProviderError) []byte {
	inner := map[string]any{
		"type":    anthropicErrorType(pe),
		"message": pe.Message,
	}
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": inner,
	})
	return body
}

// encodeResponsesAPIErrorEnvelope emits the OpenAI Responses API error
// shape. Same envelope as chat-completions visually
// (`{"error":{"message","type","code"}}`) but the `type` values follow
// Responses-API SDK conventions (see responsesAPIErrorType below).
//
// Note: the `param` field of the Responses-API error envelope (used
// by cross-format rejection to identify the offending field path) is
// emitted directly by writeResponsesFeatureRejection — not via this
// helper — because provcore.ProviderError does not carry a Param field.
// The cross-format rejection path bypasses this envelope helper.
func encodeResponsesAPIErrorEnvelope(pe *provcore.ProviderError) []byte {
	inner := map[string]any{
		"message": pe.Message,
		"type":    responsesAPIErrorType(pe),
	}
	if pe.Code != "" {
		inner["code"] = pe.Code
	}
	body, _ := json.Marshal(map[string]any{"error": inner})
	return body
}

// encodeGeminiErrorEnvelope emits the Google Cloud / Gemini shape:
//
//	{"error": {"code": <status>, "message": "...", "status": "..."}}
//
// `status` maps the HTTP status to the gRPC canonical code name so
// google-cloud-aiplatform clients can branch on it.
func encodeGeminiErrorEnvelope(pe *provcore.ProviderError) []byte {
	inner := map[string]any{
		"code":    pe.Status,
		"message": pe.Message,
		"status":  geminiStatusForHTTPCode(pe.Status),
	}
	body, _ := json.Marshal(map[string]any{"error": inner})
	return body
}

// openaiErrorType maps a normalised provider error to the OpenAI-style
// `type` field. OpenAI uses snake_case strings like "invalid_request_error"
// and "rate_limit_error" — translate from our canonical provcore.Code*
// values so OpenAI-style clients see something recognisable.
func openaiErrorType(pe *provcore.ProviderError) string {
	switch pe.Code {
	case provcore.CodeInvalidRequest:
		return "invalid_request_error"
	case provcore.CodeAuthFailed:
		return "authentication_error"
	case provcore.CodeRateLimited:
		return "rate_limit_error"
	case provcore.CodeTimeout:
		return "timeout_error"
	case provcore.CodeUpstreamError:
		return "api_error"
	case provcore.CodeEndpointUnsupported, provcore.CodeNotImplemented:
		return "invalid_request_error"
	case provcore.CodeNoCompatibleProvider:
		return "api_error"
	}
	return "api_error"
}

// responsesAPIErrorType maps provcore.ProviderError.Code to the OpenAI
// Responses-API SDK error.type enum. Sticks close to the chat-
// completions OpenAI mapping (since both shape conventions overlap on
// the type strings) but pins the values for the Responses path so a
// future divergence in OpenAI's SDK error enums lands in one place.
func responsesAPIErrorType(pe *provcore.ProviderError) string {
	switch pe.Code {
	case provcore.CodeInvalidRequest:
		return "invalid_request_error"
	case provcore.CodeAuthFailed:
		return "authentication_error"
	case provcore.CodeRateLimited:
		return "rate_limit_error"
	case provcore.CodeTimeout:
		return "timeout_error"
	case provcore.CodeUpstreamError:
		return "api_error"
	case provcore.CodeEndpointUnsupported, provcore.CodeNotImplemented:
		return "invalid_request_error"
	case provcore.CodeNoCompatibleProvider:
		return "api_error"
	case "feature_requires_native_responses_target":
		return "unsupported_feature"
	}
	return "api_error"
}

// anthropicErrorType maps a normalised provider error to the Anthropic
// `error.type` field shape so a native Anthropic SDK sees the same
// strings it would from api.anthropic.com.
func anthropicErrorType(pe *provcore.ProviderError) string {
	switch pe.Code {
	case provcore.CodeInvalidRequest:
		return "invalid_request_error"
	case provcore.CodeAuthFailed:
		return "authentication_error"
	case provcore.CodeRateLimited:
		return "rate_limit_error"
	case provcore.CodeUpstreamError:
		return "api_error"
	case provcore.CodeTimeout:
		return "api_error"
	}
	return "api_error"
}

// synthesizeSSEErrorFrame produces an SSE-formatted terminal error
// frame in the ingress format's wire shape. Mirrors
// encodeErrorEnvelopeForIngress but wraps the JSON envelope in the
// SSE-frame structure each ingress SDK expects:
//
//   - OpenAI:           `data: {"error":{...}}\n\n`
//   - OpenAI Responses: `event: response.failed\ndata: {"type":"response.failed","sequence_number":0,"response":{...}}\n\n`
//   - Anthropic:        `event: error\ndata: {"type":"error","error":{...}}\n\n`
//   - Gemini:           `data: {"error":{"code":...,"status":"..."}}\n\n`
//
// Required by provider-adapter-architecture.md §9.5: error frames must
// reach the client in the ingress envelope shape regardless of upstream.
// pe must be non-nil.
// SynthesizeSSEErrorFrame produces an SSE-formatted terminal error frame.
// Exported for use by ingress/proxy.
func SynthesizeSSEErrorFrame(ingress provcore.Format, pe *provcore.ProviderError) []byte {
	return synthesizeSSEErrorFrame(ingress, pe)
}

func synthesizeSSEErrorFrame(ingress provcore.Format, pe *provcore.ProviderError) []byte {
	if ingress == provcore.FormatOpenAIResponses {
		// Responses-API uses a dedicated `response.failed` event with a
		// structured payload (type + sequence_number + response wrapper).
		// sequence_number=0 is correct for a pre-stream or first-event
		// failure (the only path this synth fires on); when a stream
		// session has already emitted events, the synth must be invoked
		// with a counter — covered by the stream session's own failed
		// branch which threads the live counter, not by this
		// pre-stream-only helper.
		payload, _ := json.Marshal(map[string]any{
			"type":            "response.failed",
			"sequence_number": 0,
			"response": map[string]any{
				"object": "response",
				"status": "failed",
				"error": map[string]any{
					"message": pe.Message,
					"code":    pe.Code,
					"type":    responsesAPIErrorType(pe),
				},
			},
		})
		return fmt.Appendf(nil, "event: response.failed\ndata: %s\n\n", payload)
	}
	body := encodeErrorEnvelopeForIngressForStream(ingress, pe)
	if ingress == provcore.FormatAnthropic {
		return fmt.Appendf(nil, "event: error\ndata: %s\n\n", body)
	}
	return fmt.Appendf(nil, "data: %s\n\n", body)
}

// encodeErrorEnvelopeForIngressForStream is the SSE-frame counterpart
// of encodeErrorEnvelopeForIngress. It always emits a fresh ingress-
// shape envelope (never raw upstream bytes) because mid-stream there
// is no "passthrough upstream raw" option — the upstream connection
// died and we need to synthesise a frame the client can parse.
//
// Note: for FormatOpenAIResponses, synthesizeSSEErrorFrame builds a
// response.failed-shaped payload directly and does not call this
// helper; this function returns the non-stream envelope as a defensive
// fallback if any caller invokes it with Responses ingress.
func encodeErrorEnvelopeForIngressForStream(ingress provcore.Format, pe *provcore.ProviderError) []byte {
	switch ingress {
	case provcore.FormatAnthropic:
		return encodeAnthropicErrorEnvelope(pe)
	case provcore.FormatGemini, provcore.FormatVertex:
		return encodeGeminiErrorEnvelope(pe)
	case provcore.FormatOpenAIResponses:
		return encodeResponsesAPIErrorEnvelope(pe)
	default:
		return encodeOpenAIErrorEnvelope(pe)
	}
}

// geminiStatusForHTTPCode returns the gRPC canonical status name that
// Google APIs ship in the `error.status` field. We only cover the codes
// actually surfaced by our normalisers; unknown codes default to the
// gRPC UNKNOWN string so the client at least sees a defined value.
func geminiStatusForHTTPCode(code int) string {
	switch code {
	case 400:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 408, 504:
		return "DEADLINE_EXCEEDED"
	case 409:
		return "ALREADY_EXISTS"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 500:
		return "INTERNAL"
	case 501:
		return "UNIMPLEMENTED"
	case 503:
		return "UNAVAILABLE"
	}
	return "UNKNOWN"
}
