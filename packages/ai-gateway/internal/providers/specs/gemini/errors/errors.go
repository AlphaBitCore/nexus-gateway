package errors

import (
	"net/http"
	"strconv"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// ErrorNormalizer handles Gemini's standard Google API error envelope:
//
//	{"error":{"code":400,"message":"...","status":"INVALID_ARGUMENT","details":[...]}}
type ErrorNormalizer struct{}

// Normalize implements provcore.ErrorNormalizer.
func (ErrorNormalizer) Normalize(status int, headers http.Header, body []byte) *provcore.ProviderError {
	pe := &provcore.ProviderError{Status: status, Raw: body}
	errObj := gjson.GetBytes(body, "error")
	if errObj.Exists() {
		pe.Type = errObj.Get("status").String()
		pe.Message = errObj.Get("message").String()
	}
	if pe.Message == "" {
		pe.Message = http.StatusText(status)
	}
	switch pe.Type {
	case "INVALID_ARGUMENT", "FAILED_PRECONDITION":
		pe.Code = provcore.CodeInvalidRequest
	case "NOT_FOUND":
		pe.Code = provcore.CodeInvalidRequest
	case "UNAUTHENTICATED", "PERMISSION_DENIED":
		pe.Code = provcore.CodeAuthFailed
	case "RESOURCE_EXHAUSTED":
		pe.Code = provcore.CodeRateLimited
		if ra := ParseRetryAfter(headers.Get("retry-after")); ra != nil {
			pe.RetryAfter = ra
		}
	case "DEADLINE_EXCEEDED":
		pe.Code = provcore.CodeTimeout
	case "UNAVAILABLE", "INTERNAL":
		pe.Code = provcore.CodeUpstreamError
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
