package proxy

import (
	"net/http"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
)

// The SDKs pick their exception class from the HTTP status, but a consumer that
// catches the error then reads error.type — and before AP-3 every
// gateway-generated error carried the constant "proxy_error", which is not in
// OpenAI's vocabulary at all.
func TestOpenAIErrorTypeForStatus(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, "invalid_request_error"},
		{http.StatusUnauthorized, "authentication_error"},
		{http.StatusForbidden, "permission_error"},
		{http.StatusNotFound, "not_found_error"},
		{http.StatusTooManyRequests, "rate_limit_error"},
		{http.StatusBadGateway, "api_error"},
		{http.StatusInternalServerError, "api_error"},
		{http.StatusServiceUnavailable, "api_error"},
		// 4xx values OpenAI has no dedicated type for still read as caller errors.
		{http.StatusRequestTimeout, "invalid_request_error"},
		{http.StatusRequestEntityTooLarge, "invalid_request_error"},
		{http.StatusUnprocessableEntity, "invalid_request_error"},
		{statusClientClosedRequest, "invalid_request_error"},
	}
	for _, tc := range cases {
		if got := openAIErrorTypeForStatus(tc.status); got != tc.want {
			t.Errorf("status %d → %q, want %q", tc.status, got, tc.want)
		}
	}
	if got := openAIErrorTypeForStatus(http.StatusOK); got != "api_error" {
		t.Errorf("non-error status fell through to %q, want the api_error default", got)
	}
}

func TestOpenAIErrorParamForCode(t *testing.T) {
	for _, code := range []string{"MODEL_REQUIRED", "ROUTING_NO_MATCH", "MODEL_NOT_ALLOWED", "MODEL_MODALITY_MISMATCH"} {
		if got := envelope.OpenAIErrorParamForCode(code); got != "model" {
			t.Errorf("%s → param %q, want \"model\"", code, got)
		}
	}
	for _, code := range []string{"RATE_LIMITED", "QUOTA_EXCEEDED", "PROVIDER_UNAVAILABLE", "AUTH_INVALID_KEY", ""} {
		if got := envelope.OpenAIErrorParamForCode(code); got != "" {
			t.Errorf("%s should carry no param, got %q", code, got)
		}
	}
}

func TestOpenAIProxyErrorBody_ShapeAndVocabulary(t *testing.T) {
	body := openAIProxyErrorBody(http.StatusNotFound, "ROUTING_NO_MATCH",
		"no available provider for model nope", "Ensure the model exists and is enabled")

	if got := gjson.GetBytes(body, "error.type").String(); got != "not_found_error" {
		t.Errorf("error.type = %q, want not_found_error", got)
	}
	// The deliberate divergence: code stays the Nexus UPPER_SNAKE machine code.
	if got := gjson.GetBytes(body, "error.code").String(); got != "ROUTING_NO_MATCH" {
		t.Errorf("error.code = %q, want the Nexus code ROUTING_NO_MATCH", got)
	}
	if got := gjson.GetBytes(body, "error.param").String(); got != "model" {
		t.Errorf("error.param = %q, want model", got)
	}
	if got := gjson.GetBytes(body, "error.message").String(); got == "" {
		t.Error("error.message must never be empty — it is all some SDK callers surface")
	}
	if got := gjson.GetBytes(body, "error.hint").String(); got != "Ensure the model exists and is enabled" {
		t.Errorf("hint dropped: %q", got)
	}
}

// An unnamed failure omits error.code. The alternative this replaced put the
// numeric status there, which contradicted the contract the rest of the surface
// keeps — sdk_compat/test_errors.py::test_error_code_stays_nexus_upper_snake
// asserts error.code is a Nexus UPPER_SNAKE string, and /v1/rerank,
// /v1/guardrail, the 401 path and the unsupported-endpoint path all emit one.
// A caller could never have relied on a number.
func TestOpenAIProxyErrorBody_OmitsCodeWhenUnnamed(t *testing.T) {
	body := openAIProxyErrorBody(http.StatusBadRequest, "", "bad input", "")

	if got := gjson.GetBytes(body, "error.code"); got.Exists() {
		t.Errorf("error.code = %s, want it absent — the status line already carries the status", got.Raw)
	}
	if got := gjson.GetBytes(body, "error.type").String(); got != "invalid_request_error" {
		t.Errorf("error.type = %q, want invalid_request_error", got)
	}
	if gjson.GetBytes(body, "error.param").Exists() {
		t.Error("param must be absent when the code names no field")
	}
	if gjson.GetBytes(body, "error.hint").Exists() {
		t.Error("hint must be absent when empty")
	}
}

func TestOpenAIProxyErrorBody_AuthAndPermission(t *testing.T) {
	auth := openAIProxyErrorBody(http.StatusUnauthorized, "AUTH_INVALID_KEY", "virtual key invalid", "")
	if got := gjson.GetBytes(auth, "error.type").String(); got != "authentication_error" {
		t.Errorf("401 type = %q, want authentication_error", got)
	}

	denied := openAIProxyErrorBody(http.StatusForbidden, "MODEL_NOT_ALLOWED", "not allowed for this key", "")
	if got := gjson.GetBytes(denied, "error.type").String(); got != "permission_error" {
		t.Errorf("403 type = %q, want permission_error", got)
	}
	if got := gjson.GetBytes(denied, "error.param").String(); got != "model" {
		t.Errorf("403 param = %q, want model", got)
	}
}
