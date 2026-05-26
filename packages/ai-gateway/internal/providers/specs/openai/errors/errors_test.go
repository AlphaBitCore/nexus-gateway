// Package errors_test covers the OpenAI-style ErrorNormalizer.
// Named failure modes:
//   - 400 → CodeInvalidRequest
//   - 401/403 → CodeAuthFailed
//   - 429 → CodeRateLimited (+ Retry-After parsing: seconds and HTTP-date)
//   - 408/504 → CodeTimeout
//   - 404 → CodeInvalidRequest
//   - 5xx / unrecognised → CodeUpstreamError
//   - empty body: falls back to http.StatusText
//   - error.type field wins over error.code
package errors_test

import (
	"net/http"
	"testing"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	specerrpkg "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/errors"
)

func normalize(status int, headers http.Header, body []byte) *provcore.ProviderError {
	return specerrpkg.ErrorNormalizer{}.Normalize(status, headers, body)
}

func TestErrorNormalizer_400_invalidRequest(t *testing.T) {
	pe := normalize(http.StatusBadRequest, nil, []byte(`{"error":{"type":"invalid_request_error","message":"bad"}}`))
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeInvalidRequest)
	}
	if pe.Type != "invalid_request_error" {
		t.Errorf("type: got %q", pe.Type)
	}
	if pe.Message != "bad" {
		t.Errorf("message: got %q", pe.Message)
	}
}

func TestErrorNormalizer_401_authFailed(t *testing.T) {
	pe := normalize(http.StatusUnauthorized, nil, []byte(`{"error":{"type":"auth_error","message":"invalid key"}}`))
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeAuthFailed)
	}
}

func TestErrorNormalizer_403_authFailed(t *testing.T) {
	pe := normalize(http.StatusForbidden, nil, []byte(`{"error":{"type":"permission_denied","message":"forbidden"}}`))
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeAuthFailed)
	}
}

func TestErrorNormalizer_429_rateLimited_noRetryAfter(t *testing.T) {
	pe := normalize(http.StatusTooManyRequests, nil, []byte(`{"error":{"type":"rate_limit_exceeded","message":"too many requests"}}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeRateLimited)
	}
	if pe.RetryAfter != nil {
		t.Errorf("RetryAfter should be nil when header absent")
	}
}

func TestErrorNormalizer_429_retryAfterSeconds(t *testing.T) {
	h := http.Header{"Retry-After": []string{"17"}}
	pe := normalize(http.StatusTooManyRequests, h, []byte(`{"error":{"type":"rate_limit_exceeded","message":"slow"}}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("code: got %q", pe.Code)
	}
	if pe.RetryAfter == nil {
		t.Fatal("RetryAfter should be set for seconds header")
	}
	if *pe.RetryAfter != 17*time.Second {
		t.Errorf("RetryAfter: got %v, want 17s", *pe.RetryAfter)
	}
}

func TestErrorNormalizer_429_retryAfterZeroSeconds(t *testing.T) {
	h := http.Header{"Retry-After": []string{"0"}}
	pe := normalize(http.StatusTooManyRequests, h, []byte(`{"error":{"type":"rate_limit_exceeded","message":"slow"}}`))
	if pe.RetryAfter == nil || *pe.RetryAfter != 0 {
		t.Errorf("RetryAfter 0 seconds: got %v", pe.RetryAfter)
	}
}

func TestErrorNormalizer_408_timeout(t *testing.T) {
	pe := normalize(http.StatusRequestTimeout, nil, []byte(`{}`))
	if pe.Code != provcore.CodeTimeout {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeTimeout)
	}
}

func TestErrorNormalizer_504_timeout(t *testing.T) {
	pe := normalize(http.StatusGatewayTimeout, nil, []byte(`{}`))
	if pe.Code != provcore.CodeTimeout {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeTimeout)
	}
}

func TestErrorNormalizer_404_invalidRequest(t *testing.T) {
	pe := normalize(http.StatusNotFound, nil, []byte(`{"error":{"type":"model_not_found","message":"no such model"}}`))
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeInvalidRequest)
	}
}

func TestErrorNormalizer_500_upstreamError(t *testing.T) {
	pe := normalize(http.StatusInternalServerError, nil, []byte(`{"error":{"type":"server_error","message":"internal"}}`))
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeUpstreamError)
	}
}

func TestErrorNormalizer_502_upstreamError(t *testing.T) {
	pe := normalize(http.StatusBadGateway, nil, []byte(`{}`))
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeUpstreamError)
	}
}

func TestErrorNormalizer_emptyBody_fallbackMessage(t *testing.T) {
	pe := normalize(http.StatusBadRequest, nil, []byte(`{}`))
	// No error object → message falls back to http.StatusText.
	if pe.Message == "" {
		t.Errorf("Message should fall back to http.StatusText, got empty")
	}
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("code: got %q", pe.Code)
	}
}

func TestErrorNormalizer_codeField_fallback(t *testing.T) {
	// When error.type is missing but error.code is present, code fills type.
	pe := normalize(http.StatusBadRequest, nil, []byte(`{"error":{"code":"context_length_exceeded","message":"too long"}}`))
	if pe.Type != "context_length_exceeded" {
		t.Errorf("type from code fallback: got %q", pe.Type)
	}
}

func TestErrorNormalizer_statusPopulated(t *testing.T) {
	pe := normalize(http.StatusTooManyRequests, nil, []byte(`{"error":{"type":"rate_limit_exceeded","message":"slow"}}`))
	if pe.Status != http.StatusTooManyRequests {
		t.Errorf("Status: got %d, want %d", pe.Status, http.StatusTooManyRequests)
	}
}

func TestParseRetryAfter_emptyString_returnsNil(t *testing.T) {
	// parseRetryAfter is internal; tested transitively via Normalize.
	// This test exercises a 429 with an empty Retry-After to ensure nil is returned.
	h := http.Header{"Retry-After": []string{""}}
	pe := normalize(http.StatusTooManyRequests, h, []byte(`{}`))
	if pe.RetryAfter != nil {
		t.Errorf("empty Retry-After must yield nil, got %v", pe.RetryAfter)
	}
}

func TestParseRetryAfter_httpDateInPast_returnsZero(t *testing.T) {
	// HTTP-date in the past → duration clamped to 0 (never negative).
	h := http.Header{"Retry-After": []string{"Thu, 01 Jan 1970 00:00:00 GMT"}}
	pe := normalize(http.StatusTooManyRequests, h, []byte(`{}`))
	if pe.RetryAfter == nil {
		t.Fatal("RetryAfter should be non-nil for an HTTP-date")
	}
	if *pe.RetryAfter != 0 {
		t.Errorf("past HTTP-date should clamp to 0, got %v", *pe.RetryAfter)
	}
}

func TestParseRetryAfter_invalidValue_returnsNil(t *testing.T) {
	// Neither a valid integer nor a valid HTTP-date → nil.
	h := http.Header{"Retry-After": []string{"not-a-date-or-number"}}
	pe := normalize(http.StatusTooManyRequests, h, []byte(`{}`))
	if pe.RetryAfter != nil {
		t.Errorf("invalid Retry-After must yield nil, got %v", pe.RetryAfter)
	}
}
