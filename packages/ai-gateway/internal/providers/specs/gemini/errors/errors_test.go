// Package errors_test covers the Gemini ErrorNormalizer.
// Named failure modes:
//   - INVALID_ARGUMENT / FAILED_PRECONDITION → CodeInvalidRequest
//   - NOT_FOUND → CodeInvalidRequest
//   - UNAUTHENTICATED / PERMISSION_DENIED → CodeAuthFailed
//   - RESOURCE_EXHAUSTED → CodeRateLimited (+ Retry-After)
//   - DEADLINE_EXCEEDED → CodeTimeout
//   - UNAVAILABLE / INTERNAL → CodeUpstreamError
//   - Unknown type → HTTP-status fallback
//   - HTTP status fallbacks: 400/404/401/403/429/408/504/5xx
//   - ParseRetryAfter: seconds, HTTP-date, empty, invalid
package errors_test

import (
	"net/http"
	"testing"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	gemerrors "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/errors"
)

func norm(status int, headers http.Header, body []byte) *provcore.ProviderError {
	return gemerrors.ErrorNormalizer{}.Normalize(status, headers, body)
}

// Type-based mapping

func TestErrorNormalizer_invalidArgument(t *testing.T) {
	pe := norm(http.StatusBadRequest, nil,
		[]byte(`{"error":{"code":400,"message":"bad request","status":"INVALID_ARGUMENT"}}`))
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeInvalidRequest)
	}
	if pe.Type != "INVALID_ARGUMENT" {
		t.Errorf("type: got %q", pe.Type)
	}
}

func TestErrorNormalizer_failedPrecondition(t *testing.T) {
	pe := norm(http.StatusBadRequest, nil,
		[]byte(`{"error":{"code":400,"message":"precondition","status":"FAILED_PRECONDITION"}}`))
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeInvalidRequest)
	}
}

func TestErrorNormalizer_notFound(t *testing.T) {
	pe := norm(http.StatusNotFound, nil,
		[]byte(`{"error":{"code":404,"message":"not found","status":"NOT_FOUND"}}`))
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeInvalidRequest)
	}
}

func TestErrorNormalizer_unauthenticated(t *testing.T) {
	pe := norm(http.StatusUnauthorized, nil,
		[]byte(`{"error":{"code":401,"message":"bad key","status":"UNAUTHENTICATED"}}`))
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeAuthFailed)
	}
}

func TestErrorNormalizer_permissionDenied(t *testing.T) {
	pe := norm(http.StatusForbidden, nil,
		[]byte(`{"error":{"code":403,"message":"no access","status":"PERMISSION_DENIED"}}`))
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeAuthFailed)
	}
}

func TestErrorNormalizer_resourceExhausted_noRetryAfter(t *testing.T) {
	pe := norm(http.StatusTooManyRequests, nil,
		[]byte(`{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeRateLimited)
	}
	if pe.RetryAfter != nil {
		t.Error("RetryAfter should be nil when header absent")
	}
}

func TestErrorNormalizer_resourceExhausted_withRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "60")
	pe := norm(http.StatusTooManyRequests, h,
		[]byte(`{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`))
	if pe.Code != provcore.CodeRateLimited {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeRateLimited)
	}
	if pe.RetryAfter == nil {
		t.Fatal("RetryAfter should be non-nil")
	}
	if *pe.RetryAfter != 60*time.Second {
		t.Errorf("RetryAfter: got %v, want 60s", *pe.RetryAfter)
	}
}

func TestErrorNormalizer_deadlineExceeded(t *testing.T) {
	pe := norm(http.StatusGatewayTimeout, nil,
		[]byte(`{"error":{"code":504,"message":"timeout","status":"DEADLINE_EXCEEDED"}}`))
	if pe.Code != provcore.CodeTimeout {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeTimeout)
	}
}

func TestErrorNormalizer_unavailable(t *testing.T) {
	pe := norm(http.StatusServiceUnavailable, nil,
		[]byte(`{"error":{"code":503,"message":"unavailable","status":"UNAVAILABLE"}}`))
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeUpstreamError)
	}
}

func TestErrorNormalizer_internal(t *testing.T) {
	pe := norm(http.StatusInternalServerError, nil,
		[]byte(`{"error":{"code":500,"message":"internal","status":"INTERNAL"}}`))
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeUpstreamError)
	}
}

// Unknown type → HTTP fallback

func TestErrorNormalizer_unknownType_fallsBackToHTTPStatus(t *testing.T) {
	pe := norm(http.StatusInternalServerError, nil,
		[]byte(`{"error":{"code":500,"message":"something","status":"SOME_NEW_ERROR"}}`))
	// Type is preserved for observability.
	if pe.Type != "SOME_NEW_ERROR" {
		t.Errorf("type: got %q, want SOME_NEW_ERROR", pe.Type)
	}
	// Code comes from HTTP status fallback (500 → UpstreamError).
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("code: got %q, want %q", pe.Code, provcore.CodeUpstreamError)
	}
}

// HTTP status fallback (no error envelope)

func TestErrorNormalizer_statusFallback_400(t *testing.T) {
	pe := norm(http.StatusBadRequest, nil, []byte(`{}`))
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("400 fallback: got %q", pe.Code)
	}
}

func TestErrorNormalizer_statusFallback_404(t *testing.T) {
	pe := norm(http.StatusNotFound, nil, []byte(`{}`))
	if pe.Code != provcore.CodeInvalidRequest {
		t.Errorf("404 fallback: got %q", pe.Code)
	}
}

func TestErrorNormalizer_statusFallback_401(t *testing.T) {
	pe := norm(http.StatusUnauthorized, nil, []byte(`{}`))
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("401 fallback: got %q", pe.Code)
	}
}

func TestErrorNormalizer_statusFallback_403(t *testing.T) {
	pe := norm(http.StatusForbidden, nil, []byte(`{}`))
	if pe.Code != provcore.CodeAuthFailed {
		t.Errorf("403 fallback: got %q", pe.Code)
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

func TestErrorNormalizer_statusFallback_500(t *testing.T) {
	pe := norm(http.StatusInternalServerError, nil, []byte(`{}`))
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("500 fallback: got %q, want %q", pe.Code, provcore.CodeUpstreamError)
	}
}

// Status, message, raw fields

func TestErrorNormalizer_statusFieldPopulated(t *testing.T) {
	pe := norm(http.StatusBadRequest, nil,
		[]byte(`{"error":{"code":400,"message":"bad","status":"INVALID_ARGUMENT"}}`))
	if pe.Status != http.StatusBadRequest {
		t.Errorf("Status: got %d, want %d", pe.Status, http.StatusBadRequest)
	}
}

func TestErrorNormalizer_messageFromBody(t *testing.T) {
	pe := norm(http.StatusBadRequest, nil,
		[]byte(`{"error":{"code":400,"message":"specific error text","status":"INVALID_ARGUMENT"}}`))
	if pe.Message != "specific error text" {
		t.Errorf("Message: got %q, want specific error text", pe.Message)
	}
}

func TestErrorNormalizer_emptyBody_fallbackMessage(t *testing.T) {
	pe := norm(http.StatusBadRequest, nil, []byte(`{}`))
	if pe.Message == "" {
		t.Error("Message should fall back to http.StatusText")
	}
}

func TestErrorNormalizer_rawFieldPopulated(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`)
	pe := norm(http.StatusTooManyRequests, nil, body)
	if string(pe.Raw) != string(body) {
		t.Errorf("Raw: got %q, want %q", pe.Raw, body)
	}
}


func TestParseRetryAfter_emptyString_nil(t *testing.T) {
	got := gemerrors.ParseRetryAfter("")
	if got != nil {
		t.Errorf("empty string: expected nil, got %v", got)
	}
}

func TestParseRetryAfter_invalidValue_nil(t *testing.T) {
	got := gemerrors.ParseRetryAfter("not-a-number")
	if got != nil {
		t.Errorf("invalid value: expected nil, got %v", got)
	}
}

func TestParseRetryAfter_seconds(t *testing.T) {
	got := gemerrors.ParseRetryAfter("45")
	if got == nil {
		t.Fatal("expected non-nil for seconds value")
	}
	if *got != 45*time.Second {
		t.Errorf("got %v, want 45s", *got)
	}
}

func TestParseRetryAfter_zeroSeconds(t *testing.T) {
	got := gemerrors.ParseRetryAfter("0")
	if got == nil || *got != 0 {
		t.Errorf("zero seconds: got %v, want 0", got)
	}
}

func TestParseRetryAfter_pastHTTPDate_clampsToZero(t *testing.T) {
	got := gemerrors.ParseRetryAfter("Thu, 01 Jan 1970 00:00:00 GMT")
	if got == nil {
		t.Fatal("past HTTP-date should return non-nil (clamped to 0)")
	}
	if *got != 0 {
		t.Errorf("past date should clamp to 0, got %v", *got)
	}
}
