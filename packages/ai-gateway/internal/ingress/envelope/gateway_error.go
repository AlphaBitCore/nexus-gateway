package envelope

import (
	"net/http"

	"github.com/goccy/go-json"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// GatewayErrorBody builds the gateway's own OpenAI-shape error envelope.
//
// This is the single builder for every error the gateway ITSELF produces —
// refused requests, unmounted paths, catalog misses, usage-query failures.
// Errors that came from an upstream provider go through
// EncodeErrorEnvelopeForIngress instead, which preserves the provider's own
// bytes when the shapes match; the two populations answer different questions
// and are deliberately kept apart.
//
// One builder rather than one per route is the point. Five routes had grown
// five shapes — one with neither type nor code, one with a lower_snake code and
// no type, one with a "proxy_error" type that matches no SDK's vocabulary — so
// a client could not write a single error handler that worked against the
// gateway. Anything reached through here agrees by construction.
//
//   - error.code is a Nexus UPPER_SNAKE machine code, or absent. Never the
//     numeric status: the SDKs type it as an optional string, and repeating the
//     status inside the body tells a caller nothing the status line has not
//     already said while looking enough like a machine code to be matched on.
//   - error.type carries OpenAI's vocabulary, derived from the status.
//   - error.param names the offending field where one is unambiguous.
//   - error.hint carries the fix when there is one to give.
func GatewayErrorBody(status int, code, message, hint string) []byte {
	return GatewayErrorBodyWith(status, code, message, hint, nil)
}

// GatewayErrorBodyWith is GatewayErrorBody plus fields that only one refusal
// can supply — the capability rejection's available_capabilities list, the
// Responses rejection's param. Those routes built private envelopes to carry
// them, and in doing so also invented private type and code spellings; the
// extra field was the only part that needed to be different.
func GatewayErrorBodyWith(status int, code, message, hint string, extra map[string]any) []byte {
	inner := map[string]any{
		"message": message,
		"type":    OpenAIErrorTypeForStatus(status),
	}
	if code != "" {
		inner["code"] = code
	}
	if param := OpenAIErrorParamForCode(code); param != "" {
		inner["param"] = param
	}
	if hint != "" {
		inner["hint"] = hint
	}
	for k, v := range extra {
		inner[k] = v
	}
	body, _ := json.Marshal(map[string]any{"error": inner})
	return body
}

// WriteGatewayError answers a request with the gateway's own error envelope in
// the wire shape the caller's SDK expects, chosen from the request path.
//
// Routes that carry an audit record reshape by the record's ingress format
// instead — see the proxy's writeIngressError. This exists for the routes that
// have no record: the catalog, usage, and estimate surfaces.
func WriteGatewayError(w http.ResponseWriter, r *http.Request, status int, code, message, hint string) {
	// The hint goes to the builder rather than being folded into the message
	// here. Pre-folding produced two bodies for one code: RATE_LIMITED arrived
	// from this path as 'message: "rate limit exceeded (Reduce request
	// frequency…)"' and from the admission path as a separate "hint" key.
	writeInDialectWithHint(w, notSupportedFormatForPath(r.URL.Path), status, code, message, hint)
}

// hasOwnErrorDialect reports whether an ingress returns errors in a shape of
// its own rather than the OpenAI one.
//
// This is the question a GATEWAY-ORIGINATED error has to answer, and it is not
// the same question as IsOpenAIFamily. Cohere is not in the OpenAI family, but
// its rerank ingress has no error dialect either — so branching on the family
// sent a gateway error down the non-family path, where
// EncodeErrorEnvelopeForIngress's default is encodeOpenAIErrorEnvelope: the
// encoder built for UPSTREAM bodies, which stamps "param": null unconditionally
// because a real OpenAI error carries that key. Production showed the result as
// one code with two shapes — /v1/rerank answering SPEND_LIMIT_EXCEEDED with a
// null param and /v1/images/generations answering it with no param at all.
//
// Framed this way the default is safe: a format added tomorrow has no dialect
// until someone gives it one, so it reaches the gateway builder rather than the
// provider-error writer.
func hasOwnErrorDialect(format provcore.Format) bool {
	switch format {
	case provcore.FormatAnthropic, provcore.FormatGemini,
		provcore.FormatVertex, provcore.FormatOpenAIResponses:
		return true
	}
	return false
}

// GatewayErrorBodyForIngress renders an error the GATEWAY produced in the shape
// the caller's ingress expects. It is the single decision point for that
// choice; every gateway error path goes through it so no route can pick a
// different writer for the same code.
func GatewayErrorBodyForIngress(ingress provcore.Format, status int, code, message, hint string) []byte {
	if hasOwnErrorDialect(ingress) {
		msg := message
		if hint != "" {
			msg = message + " (" + hint + ")"
		}
		return EncodeErrorEnvelopeForIngress(ingress, ingress,
			&provcore.ProviderError{Status: status, Code: code, Message: msg})
	}
	return GatewayErrorBody(status, code, message, hint)
}

// writeInDialect emits one gateway error in the shape the given ingress expects.
func writeInDialect(w http.ResponseWriter, format provcore.Format, status int, pe *provcore.ProviderError) {
	writeInDialectWithHint(w, format, status, pe.Code, pe.Message, "")
}

// writeInDialectWithHint is writeInDialect for the paths that carry a hint. The
// builder decides where it lands: its own key in the OpenAI shape, appended to
// the message in a dialect whose envelope has nowhere to put it.
func writeInDialectWithHint(w http.ResponseWriter, format provcore.Format, status int, code, message, hint string) {
	body := GatewayErrorBodyForIngress(format, status, code, message, hint)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// OpenAIErrorTypeForStatus maps an HTTP status onto OpenAI's error.type
// vocabulary. Deriving from the status rather than from a per-code table is
// deliberate: the gateway emits ~45 distinct UPPER_SNAKE codes across the
// chat / embeddings / STT / video / realtime / guardrail paths, and a table
// would silently fall back to a wrong default every time a new one is added.
// The status is what OpenAI's own vocabulary tracks, and it is always set.
func OpenAIErrorTypeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	}
	if status >= 500 {
		return "api_error"
	}
	// 4xx values OpenAI has no dedicated type for (408, 409, 413, 422, 499 …)
	// are still caller errors, which is what invalid_request_error means.
	if status >= 400 {
		return "invalid_request_error"
	}
	return "api_error"
}

// OpenAIErrorParamForCode names the offending request field for the codes where
// one is unambiguous. OpenAI populates error.param so a client can point at the
// input that failed; absent for codes that are not about a single field.
func OpenAIErrorParamForCode(code string) string {
	switch code {
	case "MODEL_REQUIRED", "ROUTING_NO_MATCH", "MODEL_NOT_ALLOWED", "MODEL_MODALITY_MISMATCH":
		return "model"
	}
	return ""
}
