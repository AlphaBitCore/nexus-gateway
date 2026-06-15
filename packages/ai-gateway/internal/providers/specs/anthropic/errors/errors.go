package errors

import (
	"net/http"
	"strconv"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// errorNormalizer handles Anthropic's error envelope:
//
//	{"type":"error","error":{"type":"rate_limit_error","message":"..."}}
type ErrorNormalizer struct{}

// MapErrorType maps an Anthropic error-envelope `type` to the canonical
// provider error code and the HTTP status the gateway surfaces. It is the
// single source of truth shared by the unary HTTP normaliser (Normalize)
// and the streaming `error`-event path (stream.MapAnthropicStreamError) so
// the same upstream error class yields an identical canonical code — and
// therefore an identical retry/failover classification — regardless of
// whether it arrived in the response body or an SSE frame.
// ok=false means the type is unrecognised; callers fall back to
// status-based mapping (unary) or upstream_error/502 (stream).
//
// overloaded_error maps to rate_limited (the retryable bucket): Anthropic's
// 529 overload is transient and SHOULD be retried, matching how the gateway
// treats rate_limit_error.
func MapErrorType(etype string) (code string, status int, ok bool) {
	switch etype {
	case "authentication_error", "permission_error":
		return provcore.CodeAuthFailed, http.StatusUnauthorized, true
	case "invalid_request_error":
		return provcore.CodeInvalidRequest, http.StatusBadRequest, true
	case "not_found_error":
		return provcore.CodeInvalidRequest, http.StatusNotFound, true
	case "rate_limit_error", "overloaded_error":
		return provcore.CodeRateLimited, http.StatusTooManyRequests, true
	case "api_error":
		return provcore.CodeUpstreamError, http.StatusBadGateway, true
	}
	return "", 0, false
}

// Normalize implements provcore.ErrorNormalizer.
func (ErrorNormalizer) Normalize(status int, headers http.Header, body []byte) *provcore.ProviderError {
	pe := &provcore.ProviderError{Status: status, Raw: body}

	errObj := gjson.GetBytes(body, "error")
	if errObj.Exists() {
		pe.Type = errObj.Get("type").String()
		pe.Message = errObj.Get("message").String()
	}
	if pe.Message == "" {
		pe.Message = http.StatusText(status)
	}

	if code, _, ok := MapErrorType(pe.Type); ok {
		pe.Code = code
		if code == provcore.CodeRateLimited {
			if ra := ParseRetryAfter(headers.Get("retry-after")); ra != nil {
				pe.RetryAfter = ra
			}
		}
	}
	if pe.Code == "" {
		switch status {
		case http.StatusBadRequest, http.StatusNotFound:
			pe.Code = provcore.CodeInvalidRequest
		case http.StatusUnauthorized, http.StatusForbidden:
			pe.Code = provcore.CodeAuthFailed
		case http.StatusTooManyRequests:
			pe.Code = provcore.CodeRateLimited
			if ra := ParseRetryAfter(headers.Get("retry-after")); ra != nil {
				pe.RetryAfter = ra
			}
		case http.StatusRequestTimeout, http.StatusGatewayTimeout:
			pe.Code = provcore.CodeTimeout
		default:
			pe.Code = provcore.CodeUpstreamError
		}
	}
	return pe
}

// ParseRetryAfter parses a Retry-After header value (seconds integer or HTTP-date)
// into a Duration. Returns nil if the value is empty or unparseable.
func ParseRetryAfter(v string) *time.Duration {
	if v == "" {
		return nil
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		d := time.Duration(secs) * time.Second
		return &d
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return &d
	}
	return nil
}
