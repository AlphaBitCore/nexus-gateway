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

// Context-overflow classification: Anthropic signals an over-window
// prompt as invalid_request_error with the message "prompt is too long:
// N tokens > M maximum" (observed on claude 400s). It must map to
// CodeContextOverflow, not the terminal invalid_request bucket.
func TestNormalize_PromptTooLong_MapsToContextOverflow(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210245 tokens > 200000 maximum"}}`)
	pe := anterrors.ErrorNormalizer{}.Normalize(400, http.Header{}, body)
	if pe.Code != provcore.CodeContextOverflow {
		t.Errorf("Code = %q, want %q", pe.Code, provcore.CodeContextOverflow)
	}
}

// Account quota exhaustion arrives as invalid_request_error — Anthropic
// returns HTTP 400 for it, the same envelope a malformed body gets. Left
// there it is the caller's fault by definition: no other target would answer
// differently, so the executor rightly aborts, and a request that had three
// healthy alternates on other providers fails anyway.
//
// Measured on a live deployment: `model: auto` chose an Anthropic model, the
// routing trace carried deepseek, openai and google-gemini alternates, and the
// caller got this 400 with no failover attempted.
//
// Same shape as the context-overflow reclassification directly above it: an
// upstream that files a provider-side condition under the caller's envelope.
func TestNormalize_UsageLimitIsNotACallerError(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"You have reached your specified API usage limits. You will regain access on 2026-09-01 at 00:00 UTC."}}`)

	pe := anterrors.ErrorNormalizer{}.Normalize(http.StatusBadRequest, http.Header{}, body)

	if pe.Code == provcore.CodeInvalidRequest {
		t.Errorf("code = %q — a spent account budget is the provider's state, not a malformed request; classifying it as the caller's fault is what stops the failover", pe.Code)
	}
	if pe.Code != provcore.CodeProviderQuotaExhausted {
		t.Errorf("code = %q, want %q", pe.Code, provcore.CodeProviderQuotaExhausted)
	}
}

// The neighbouring conditions must keep their own classification: a genuinely
// malformed body is still the caller's fault, and a rate limit is still the
// transient bucket. A matcher loose enough to swallow either would trade one
// misclassification for two.
func TestNormalize_UsageLimitMatcherDoesNotSwallowItsNeighbours(t *testing.T) {
	cases := []struct {
		name, body string
		want       string
	}{
		{"malformed body", `{"type":"error","error":{"type":"invalid_request_error","message":"messages: at least one message is required"}}`, provcore.CodeInvalidRequest},
		{"context overflow", `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 300000 tokens > 200000 maximum"}}`, provcore.CodeContextOverflow},
		{"rate limit", `{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`, provcore.CodeRateLimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pe := anterrors.ErrorNormalizer{}.Normalize(http.StatusBadRequest, http.Header{}, []byte(tc.body))
			if pe.Code != tc.want {
				t.Errorf("code = %q, want %q", pe.Code, tc.want)
			}
		})
	}
}
