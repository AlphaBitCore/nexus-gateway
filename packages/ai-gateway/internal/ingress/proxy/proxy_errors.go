package proxy

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// proxy_errors.go holds the gateway-generated error writers and provider-error
// extraction helpers split out of proxy.go (behavior-unchanged relocation).

// statusClientClosedRequest mirrors nginx's 499: the client closed the
// connection (or its request context was canceled) before the gateway produced
// a response. It is NOT a provider failure — recording it as 502
// PROVIDER_UNAVAILABLE would blame the upstream for a client-side disconnect and
// pollute provider-availability metrics. Go's net/http defines no 499 constant.
const statusClientClosedRequest = 499

// errModelRequired is the typed error for a body with no `model`. Typed rather
// than a bare errors.New so writeCodecErr preserves the named code — the caller
// then learns WHICH field was missing (MODEL_REQUIRED + error.param "model")
// instead of a 400 whose only code was the numeric status.
var errModelRequired = &provcore.ProviderError{
	Status:  http.StatusBadRequest,
	Code:    "MODEL_REQUIRED",
	Message: "model is required",
}

// writeError emits a gateway-generated failure under a named machine code.
//
// The traffic row's error_code is ours to name, and until it was required here
// every caller left it empty: production carried 400s and 502s with no
// machine-readable cause at all — a /v1/responses 400 and a
// /v1/audio/transcriptions 400 with neither code nor reason, and a /v1/rerank
// 502 whose only identity was the English sentence "upstream response could not
// be reshaped for ingress format". Nothing could group, alert on, or count them.
// Taking the code as a required argument rather than defaulting it means a new
// failure path cannot be added without naming itself.
//
// The caller gets that same code. It reaches the traffic row and the response
// body from one argument because they answer the same question — what was
// refused — and a caller who has to branch on the failure needs the answer at
// least as much as the operator reading it back afterwards.
func (h *Handler) writeError(w http.ResponseWriter, rec *audit.Record, status int, auditCode, message string) {
	h.writeIngressError(w, rec, status, auditCode, message, "")
}

func (h *Handler) writeDetailedErr(w http.ResponseWriter, rec *audit.Record, status int, code, message, hint string) {
	h.writeIngressError(w, rec, status, code, message, hint)
}

// gatewayErrorCode translates a canonical code into the caller-facing
// vocabulary. It is the one place the two namespaces meet.
//
// The canonical codes are lower_snake because they describe what an UPSTREAM
// did, and they are the Hub's shared vocabulary. But the codec and canonical
// layers also raise them for refusals the gateway makes BEFORE dispatch — 82
// sites name provcore.CodeInvalidRequest for a body this provider will not
// accept — and those reach a caller through the gateway's own envelope, whose
// contract is UPPER_SNAKE. Production showed both spellings one second apart:
// an unrecognised content part answered "invalid_request" while the refusal
// beside it answered REDACT_INFLIGHT_UNSUPPORTED.
//
// Translating here rather than at each raise site is deliberate. The 82 sites
// are correct to speak the canonical vocabulary — that is the language of the
// layer they live in, and the Hub reads it. What was missing was the boundary.
// provcore.CodeSpendLimitExceeded is the existing evidence for this reading:
// it is UPPER_SNAKE alone among its neighbours, and its comment says why —
// "it reaches a CALLER through the gateway's own error envelope".
//
// A code already in the gateway's spelling passes through untouched, so a
// writer that names itself is never second-guessed.
func gatewayErrorCode(canonical string) string {
	switch canonical {
	case "":
		return "INVALID_REQUEST"
	case provcore.CodeInvalidRequest:
		return "INVALID_REQUEST"
	case provcore.CodeAuthFailed:
		return "AUTH_FAILED"
	case provcore.CodeRateLimited:
		return "RATE_LIMITED"
	case provcore.CodeProviderQuotaExhausted:
		return "PROVIDER_QUOTA_EXHAUSTED"
	case provcore.CodeTimeout:
		return "UPSTREAM_TIMEOUT"
	case provcore.CodeUpstreamError:
		return "UPSTREAM_ERROR"
	case provcore.CodeEndpointUnsupported:
		return "ENDPOINT_NOT_SUPPORTED"
	case provcore.CodeContextOverflow:
		return "CONTEXT_OVERFLOW"
	case provcore.CodeNotImplemented:
		return "NOT_IMPLEMENTED"
	case provcore.CodeNoCompatibleProvider:
		return "NO_COMPATIBLE_PROVIDER"
	}
	return canonical
}

// writeCodecErr writes an error from the codec / prepare-body path, preserving a
// typed *provcore.ProviderError's Status and Type so a codec Fail is not
// flattened to a generic 400 that mislabels a non-400 codec error and drops the
// type. Its Code is translated into the caller-facing vocabulary by
// gatewayErrorCode above — the codec layer speaks canonical lower_snake, the
// caller is owed UPPER_SNAKE, and this is the boundary. An untyped error (plain
// fmt.Errorf: missing model, empty body) falls back to a 400 with
// fallbackPrefix for context. A typed error that set neither Status nor Code
// still yields a valid response: 400 INVALID_REQUEST.
func (h *Handler) writeCodecErr(w http.ResponseWriter, rec *audit.Record, err error, fallbackPrefix string) {
	var pe *provcore.ProviderError
	if errors.As(err, &pe) {
		status := pe.Status
		if status == 0 {
			status = http.StatusBadRequest
		}
		h.writeDetailedErr(w, rec, status, gatewayErrorCode(pe.Code), pe.Message, "")
		return
	}
	h.writeError(w, rec, http.StatusBadRequest, "CODEC_ENCODE_FAILED", fallbackPrefix+err.Error())
}

// writeIngressError emits a gateway-generated error in the CALLER's ingress wire
// shape (B→canonical→A applied to the error path: anthropic /v1/messages →
// {"type":"error",...}, gemini /v1beta → {"error":{code,...}}, /v1/responses →
// Responses error shape; OpenAI-family + unknown → the OpenAI error shape)
// AND ALWAYS stamps the emitted body onto rec.ResponseBody so the error lands in
// traffic_event.payloads.response_body for Traffic-drawer triage — errors are
// captured unconditionally, independent of the StoreResponseBody payload gate,
// because a gateway-generated error envelope carries no user content and is the
// single most useful thing to see when a request fails.
func (h *Handler) writeIngressError(w http.ResponseWriter, rec *audit.Record, status int, code, message, hint string) {
	rec.StatusCode = status
	if code != "" {
		rec.ErrorCode = code
	}
	rec.ErrorReason = message

	// One decision point for "which shape does a GATEWAY error take here", shared
	// with the envelope package's own writer. Branching on IsOpenAIFamily was
	// the mistake: cohere, bedrock, replicate and voyage are outside that family
	// and have no error dialect either, so they fell through to the encoder
	// built for UPSTREAM bodies and picked up its unconditional "param": null.
	body := envelope.GatewayErrorBodyForIngress(
		provcore.Format(rec.IngressFormat), status, code, message, hint)

	rec.ResponseBody = body
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// openAIProxyErrorBody delegates to the single gateway-error builder. The
// wrapper stays so the proxy's own error paths read at one level of detail.
func openAIProxyErrorBody(status int, code, message, hint string) []byte {
	return envelope.GatewayErrorBody(status, code, message, hint)
}

func openAIErrorTypeForStatus(status int) string {
	return envelope.OpenAIErrorTypeForStatus(status)
}

// extractProviderErrorMessage extracts a human-readable error message from a
// provider response body. Handles the common JSON envelope used by OpenAI,
// Anthropic, and Gemini (.error.message or top-level .message). Falls back to
// a truncated raw body, or a generic "provider returned HTTP <N>" when empty.
func extractProviderErrorMessage(body []byte, statusCode int) string {
	if len(body) == 0 {
		return fmt.Sprintf("provider returned HTTP %d", statusCode)
	}
	if msg := gjson.GetBytes(body, "error.message").String(); msg != "" {
		return msg
	}
	if msg := gjson.GetBytes(body, "message").String(); msg != "" {
		return msg
	}
	if len(body) > 300 {
		return string(body[:300]) + "..."
	}
	return string(body)
}
