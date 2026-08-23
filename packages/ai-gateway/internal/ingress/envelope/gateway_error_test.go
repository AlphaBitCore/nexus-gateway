package envelope

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// One shape for every error the gateway itself produces.
//
// Five routes had grown five: the catalog answered with neither a type nor a
// code, estimate with a lower_snake code and no type, usage with a
// "proxy_error" type that is in no SDK's vocabulary, the proxy with the numeric
// status as its code, and any unmounted path outside /v1 with text/plain. A
// caller could not write one error handler that worked against the gateway.
func TestGatewayErrorBody_HoldsTheSameShapeAtEveryStatus(t *testing.T) {
	for _, tc := range []struct {
		status   int
		code     string
		wantType string
		wantsPar bool
	}{
		{400, "MODEL_REQUIRED", "invalid_request_error", true},
		{401, "AUTH_KEY_MISSING", "authentication_error", false},
		{403, "MODEL_NOT_ALLOWED", "permission_error", true},
		{404, "MODEL_NOT_FOUND", "not_found_error", false},
		{404, "ENDPOINT_NOT_SUPPORTED", "not_found_error", false},
		{405, "ESTIMATE_METHOD_NOT_ALLOWED", "invalid_request_error", false},
		{429, "ESTIMATE_COMPARE_RATE_LIMITED", "rate_limit_error", false},
		{500, "USAGE_QUERY_FAILED", "api_error", false},
		{502, "PROVIDER_UNAVAILABLE", "api_error", false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			var env struct {
				Error map[string]any `json:"error"`
			}
			if err := json.Unmarshal(GatewayErrorBody(tc.status, tc.code, "refused", ""), &env); err != nil {
				t.Fatalf("not JSON: %v", err)
			}
			if got, _ := env.Error["code"].(string); got != tc.code {
				t.Errorf("error.code = %v, want the Nexus machine code %q", env.Error["code"], tc.code)
			}
			if got, _ := env.Error["type"].(string); got != tc.wantType {
				t.Errorf("error.type = %q, want %q", got, tc.wantType)
			}
			if got, _ := env.Error["message"].(string); got != "refused" {
				t.Errorf("error.message = %q", got)
			}
			if _, has := env.Error["param"]; has != tc.wantsPar {
				t.Errorf("error.param present = %v, want %v", has, tc.wantsPar)
			}
		})
	}
}

// The dialect comes from the path, because these routes have no audit record to
// read an ingress format from. An SDK that only understands its own envelope
// cannot parse another's, which is the same failure as text/plain in a
// different costume.
func TestWriteGatewayError_AnswersInTheCallersDialect(t *testing.T) {
	for _, tc := range []struct {
		path      string
		wantAtTop string
		wantInner map[string]string
	}{
		{path: "/v1/models/x", wantInner: map[string]string{"type": "not_found_error", "code": "MODEL_NOT_FOUND"}},
		{path: "/v1/messages", wantAtTop: "error", wantInner: map[string]string{"type": "not_found_error"}},
		{path: "/v1beta/models/x:generateContent", wantInner: map[string]string{"status": "NOT_FOUND"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			WriteGatewayError(rr, httptest.NewRequest(http.MethodGet, tc.path, nil),
				http.StatusNotFound, "MODEL_NOT_FOUND", "model not found: x", "")

			if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q", ct)
			}
			var env struct {
				Type  string         `json:"type"`
				Error map[string]any `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("not JSON: %v (%s)", err, rr.Body)
			}
			if env.Type != tc.wantAtTop {
				t.Errorf("top-level type = %q, want %q — an Anthropic SDK keys on it", env.Type, tc.wantAtTop)
			}
			for k, want := range tc.wantInner {
				if got, _ := env.Error[k].(string); got != want {
					t.Errorf("error.%s = %q, want %q (body %s)", k, got, want, rr.Body)
				}
			}
			if s, _ := env.Error["message"].(string); s == "" {
				t.Errorf("no message: %s", rr.Body)
			}
		})
	}
}

// An error whose code is outside the canonical provider set still has a status,
// and the status is what both vocabularies track.
//
// The three mappers key off provcore.Code* and used to return a blanket
// api_error for anything else — which typed a 404 as a server fault, and
// covered every gateway-generated error, since those all carry a Nexus
// UPPER_SNAKE code rather than a canonical one. Every pre-existing table for
// these mappers builds its ProviderError with Status 0, where the fallback and
// the old default agree, so none of them could tell the two apart.
func TestErrorTypeMappers_FallBackToTheStatusNotToAServerFault(t *testing.T) {
	for _, tc := range []struct {
		status            int
		openai, anthropic string
		responses         string
	}{
		{404, "not_found_error", "not_found_error", "not_found_error"},
		{401, "authentication_error", "authentication_error", "authentication_error"},
		{403, "permission_error", "permission_error", "permission_error"},
		{429, "rate_limit_error", "rate_limit_error", "rate_limit_error"},
		{502, "api_error", "api_error", "api_error"},
	} {
		pe := &provcore.ProviderError{Code: "ENDPOINT_NOT_SUPPORTED", Status: tc.status, Message: "x"}
		if got := openaiErrorType(pe); got != tc.openai {
			t.Errorf("status %d: openaiErrorType = %q, want %q", tc.status, got, tc.openai)
		}
		if got := anthropicErrorType(pe); got != tc.anthropic {
			t.Errorf("status %d: anthropicErrorType = %q, want %q", tc.status, got, tc.anthropic)
		}
		if got := responsesAPIErrorType(pe); got != tc.responses {
			t.Errorf("status %d: responsesAPIErrorType = %q, want %q", tc.status, got, tc.responses)
		}
	}
}

// Quota exhaustion is named rather than left to the status. The upstream sends
// it as a 400, so the fallback would read it as the caller's malformed request
// when the request was fine and the provider's account is not.
func TestErrorTypeMappers_QuotaExhaustionIsNotTheCallersFault(t *testing.T) {
	pe := &provcore.ProviderError{Code: provcore.CodeProviderQuotaExhausted, Status: 400, Message: "x"}
	for name, got := range map[string]string{
		"openai":    openaiErrorType(pe),
		"anthropic": anthropicErrorType(pe),
		"responses": responsesAPIErrorType(pe),
	} {
		if got != "api_error" {
			t.Errorf("%s: %q, want api_error", name, got)
		}
	}
}

// A 405 is a case we know exactly, and the Gemini envelope was shrugging at it.
//
// Measured on production after the 405 restoration landed:
//
//	GET /v1beta/models/gemini-2.5-flash:generateContent
//	→ 405 {"code":405,"message":"GET is not allowed …","status":"UNKNOWN"}
//
// A Gemini client reads `status` as the canonical code; UNKNOWN tells it
// nothing, and specifically does not say the method was wrong. 405 is absent
// from Google's documented HTTP→gRPC table, so the honest read is the one the
// OpenAI shape already gives the same case: the caller's request was wrong for
// this endpoint.
func TestGeminiStatusForHTTPCode_KnowsTheCasesWeProduce(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{
		{400, "INVALID_ARGUMENT"},
		{401, "UNAUTHENTICATED"},
		{403, "PERMISSION_DENIED"},
		{404, "NOT_FOUND"},
		{405, "INVALID_ARGUMENT"},
		{429, "RESOURCE_EXHAUSTED"},
		{500, "INTERNAL"},
	} {
		if got := geminiStatusForHTTPCode(tc.code); got != tc.want {
			t.Errorf("%d → %q, want %q", tc.code, got, tc.want)
		}
	}
	// A status nothing here produces still falls through honestly rather than
	// being invented.
	if got := geminiStatusForHTTPCode(418); got != "UNKNOWN" {
		t.Errorf("418 → %q, want the UNKNOWN fallback for a case we do not produce", got)
	}
}
