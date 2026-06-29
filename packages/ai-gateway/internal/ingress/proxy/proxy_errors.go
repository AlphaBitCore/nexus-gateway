package proxy

import (
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"strings"

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

func (h *Handler) writeError(w http.ResponseWriter, rec *audit.Record, status int, message string) {
	h.writeIngressError(w, rec, status, "", message, "")
}

func (h *Handler) writeDetailedErr(w http.ResponseWriter, rec *audit.Record, status int, code, message, hint string) {
	h.writeIngressError(w, rec, status, code, message, hint)
}

// writeIngressError emits a gateway-generated error in the CALLER's ingress wire
// shape (B→canonical→A applied to the error path: anthropic /v1/messages →
// {"type":"error",...}, gemini /v1beta → {"error":{code,...}}, /v1/responses →
// Responses error shape; OpenAI-family + unknown → the OpenAI proxy_error shape)
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

	var body []byte
	ingressFmt := provcore.Format(rec.IngressFormat)
	if ingressFmt != "" && !ingressFmt.IsOpenAIFamily() {
		// Non-OpenAI ingress (anthropic / gemini / vertex / openai-responses):
		// reshape the error to the ingress envelope. Same-ingress synthetic
		// error (no upstream Raw) falls through to the per-format encoder.
		msg := message
		if hint != "" {
			msg = message + " (" + hint + ")"
		}
		body = envelope.EncodeErrorEnvelopeForIngress(ingressFmt, ingressFmt,
			&provcore.ProviderError{Status: status, Code: code, Message: msg})
	} else {
		body = openAIProxyErrorBody(status, code, message, hint)
	}

	rec.ResponseBody = body
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// openAIProxyErrorBody builds the gateway's OpenAI-shape proxy error envelope.
// When code is empty the numeric status is used as the code (legacy
// writeJSONError behaviour); otherwise the string code is used (legacy
// writeDetailedError behaviour). hint rides along when present.
func openAIProxyErrorBody(status int, code, message, hint string) []byte {
	inner := map[string]any{"message": message, "type": "proxy_error"}
	if code != "" {
		inner["code"] = code
	} else {
		inner["code"] = status
	}
	if hint != "" {
		inner["hint"] = hint
	}
	resp, _ := json.Marshal(map[string]any{"error": inner})
	return resp
}

// geminicacheStaleRefError reports whether a Gemini 403 response body
// carries the stale-cachedContent error signature. Gemini phrases the
// message a few ways across API versions ("CachedContent not found",
// "permission denied" with the cache name, "GenerateContentRequest:
// cachedContent not found"); we match on the substrings that are
// stable across all of them, keeping false-positives low.
func geminicacheStaleRefError(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	// gjson would be more rigorous but the error payload shape varies;
	// substring match on the lowercase body is robust to wrapping.
	low := strings.ToLower(string(body))
	if strings.Contains(low, "cachedcontent not found") ||
		strings.Contains(low, "cached content not found") ||
		strings.Contains(low, "cachedcontents/") && strings.Contains(low, "not found") ||
		strings.Contains(low, "cachedcontents/") && strings.Contains(low, "permission denied") {
		return true
	}
	return false
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
