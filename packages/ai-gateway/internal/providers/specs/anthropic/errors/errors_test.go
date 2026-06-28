// Package errors_test covers the Anthropic-specific ErrorNormalizer.
// Named failure modes:
//   - authentication_error / permission_error → CodeAuthFailed
//   - invalid_request_error → CodeInvalidRequest
//   - rate_limit_error → CodeRateLimited (+ Retry-After: seconds and HTTP-date)
//   - overloaded_error / api_error → CodeUpstreamError
//   - not_found_error → CodeInvalidRequest
//   - unknown type falls through to HTTP-status-based mapping
//   - 401/403 HTTP status fallback (no type in body)
//   - 408/504 timeout fallback
//   - 5xx fallback → CodeUpstreamError
//   - Retry-After: invalid value → nil
package errors_test

import (
	"net/http"
	"testing"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	anterrors "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/anthropic/errors"
)

func norm(status int, headers http.Header, body []byte) *provcore.ProviderError {
	return anterrors.ErrorNormalizer{}.Normalize(status, headers, body)
}

// Type-based mapping

func TestErrorNormalizer_authenticationError(t *testing.T) {
	pe := norm(http.StatusUnauthorized, nil,
		[]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid key"}}`))
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeAuthFailed)
	}
	if pe.Type != "authentication_error" {
		t.Errorf("type: got %q", pe.Type)
	}
}

func TestErrorNormalizer_permissionError(t *testing.T) {
	pe := norm(http.StatusForbidden, nil,
		[]byte(`{"type":"error","error":{"type":"permission_error","message":"no access"}}`))
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeAuthFailed)
	}
}

func TestErrorNormalizer_invalidRequestError(t *testing.T) {
	pe := norm(http.StatusBadRequest, nil,
		[]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad model"}}`))
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeInvalidRequest)
	}
}

func TestErrorNormalizer_rateLimitError_noRetryAfter(t *testing.T) {
	pe := norm(http.StatusTooManyRequests, nil,
		[]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeRateLimited)
	}
	if pe.RetryAfter != nil {
		t.Errorf("RetryAfter should be nil when header absent")
	}
}

func TestErrorNormalizer_rateLimitError_retryAfterSeconds(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "30")
	pe := norm(http.StatusTooManyRequests, h,
		[]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("code: got %q", pe.Code)
	}
	if pe.RetryAfter == nil {
		t.Fatal("RetryAfter should be non-nil")
	}
	if *pe.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter: got %v, want 30s", *pe.RetryAfter)
	}
}

func TestErrorNormalizer_overloadedError(t *testing.T) {
	// overloaded_error maps to rate_limited (the retryable bucket),
	// consistent with the streaming path. Anthropic's 529 overload is
	// transient and should be retried.
	pe := norm(http.StatusServiceUnavailable, nil,
		[]byte(`{"type":"error","error":{"type":"overloaded_error","message":"server busy"}}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeRateLimited)
	}
}

func TestErrorNormalizer_apiError(t *testing.T) {
	pe := norm(http.StatusInternalServerError, nil,
		[]byte(`{"type":"error","error":{"type":"api_error","message":"internal error"}}`))
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeUpstreamError)
	}
}

func TestErrorNormalizer_notFoundError(t *testing.T) {
	pe := norm(http.StatusNotFound, nil,
		[]byte(`{"type":"error","error":{"type":"not_found_error","message":"no model"}}`))
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeInvalidRequest)
	}
}

// HTTP status fallback (when type is absent or unknown)

func TestErrorNormalizer_statusFallback_401(t *testing.T) {
	pe := norm(http.StatusUnauthorized, nil, []byte(`{}`))
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("401 fallback: got %q, want %q", pe.Code, provcore.CodeAuthFailed)
	}
}

func TestErrorNormalizer_statusFallback_403(t *testing.T) {
	pe := norm(http.StatusForbidden, nil, []byte(`{}`))
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("403 fallback: got %q, want %q", pe.Code, provcore.CodeAuthFailed)
	}
}

func TestErrorNormalizer_statusFallback_400(t *testing.T) {
	pe := norm(http.StatusBadRequest, nil, []byte(`{}`))
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("400 fallback: got %q, want %q", pe.Code, provcore.CodeInvalidRequest)
	}
}

func TestErrorNormalizer_statusFallback_404(t *testing.T) {
	pe := norm(http.StatusNotFound, nil, []byte(`{}`))
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("404 fallback: got %q, want %q", pe.Code, provcore.CodeInvalidRequest)
	}
}

func TestErrorNormalizer_statusFallback_429_withRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "5")
	pe := norm(http.StatusTooManyRequests, h, []byte(`{}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("429 fallback: got %q", pe.Code)
	}
	if pe.RetryAfter == nil || *pe.RetryAfter != 5*time.Second {
		t.Errorf("RetryAfter: got %v, want 5s", pe.RetryAfter)
	}
}

func TestErrorNormalizer_statusFallback_408_timeout(t *testing.T) {
	pe := norm(http.StatusRequestTimeout, nil, []byte(`{}`))
	if pe.Code != provcore.CodeTimeout {
		t.Errorf("408 fallback: got %q, want %q", pe.Code, provcore.CodeTimeout)
	}
}

func TestErrorNormalizer_statusFallback_504_timeout(t *testing.T) {
	pe := norm(http.StatusGatewayTimeout, nil, []byte(`{}`))
	if pe.Code != provcore.CodeTimeout {
		t.Errorf("504 fallback: got %q, want %q", pe.Code, provcore.CodeTimeout)
	}
}

func TestErrorNormalizer_statusFallback_500_upstreamError(t *testing.T) {
	pe := norm(http.StatusInternalServerError, nil, []byte(`{}`))
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("500 fallback: got %q, want %q", pe.Code, provcore.CodeUpstreamError)
	}
}

func TestParseRetryAfter_emptyString_nil(t *testing.T) {
	got := anterrors.ParseRetryAfter("")
	if got != nil {
		t.Errorf("empty string: expected nil, got %v", got)
	}
}

func TestParseRetryAfter_invalidValue_nil(t *testing.T) {
	got := anterrors.ParseRetryAfter("not-a-number-or-date")
	if got != nil {
		t.Errorf("invalid value: expected nil, got %v", got)
	}
}

func TestParseRetryAfter_pastHTTPDate_zeroOrNil(t *testing.T) {
	got := anterrors.ParseRetryAfter("Thu, 01 Jan 1970 00:00:00 GMT")
	if got == nil {
		t.Fatal("past HTTP-date should return non-nil (clamped to 0)")
	}
	if *got != 0 {
		t.Errorf("past date should clamp to 0, got %v", *got)
	}
}

func TestParseRetryAfter_zeroSeconds(t *testing.T) {
	got := anterrors.ParseRetryAfter("0")
	if got == nil || *got != 0 {
		t.Errorf("zero seconds: got %v, want 0", got)
	}
}

// Status and message fields

func TestErrorNormalizer_statusFieldPopulated(t *testing.T) {
	pe := norm(http.StatusBadRequest, nil,
		[]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`))
	if pe.Status != http.StatusBadRequest {
		t.Errorf("Status: got %d, want %d", pe.Status, http.StatusBadRequest)
	}
}

func TestErrorNormalizer_emptyBody_fallbackMessage(t *testing.T) {
	pe := norm(http.StatusBadRequest, nil, []byte(`{}`))
	if pe.Message == "" {
		t.Error("Message should fall back to http.StatusText")
	}
}

func TestErrorNormalizer_rawFieldPopulated(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`)
	pe := norm(http.StatusTooManyRequests, nil, body)
	if string(pe.Raw) != string(body) {
		t.Errorf("Raw: got %q, want %q", pe.Raw, body)
	}
}

func TestErrorNormalizer_unknownType_httpStatusFallback(t *testing.T) {
	// An unknown error type should fall through to HTTP status mapping.
	pe := norm(http.StatusInternalServerError, nil,
		[]byte(`{"type":"error","error":{"type":"some_new_unknown_error","message":"weird"}}`))
	// Type is preserved for observability.
	if pe.Type != "some_new_unknown_error" {
		t.Errorf("type: got %q, want some_new_unknown_error", pe.Type)
	}
	// Code comes from HTTP status fallback.
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeUpstreamError)
	}
}
