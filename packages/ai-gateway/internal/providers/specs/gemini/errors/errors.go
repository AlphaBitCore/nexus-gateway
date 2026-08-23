package errors

import (
	"net/http"
	"strconv"
	"strings"
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
		// Context overflow arrives as 400 INVALID_ARGUMENT with the
		// message "The input token count (N) exceeds the maximum number
		// of tokens allowed (M)" (observed on gemini-2.x 400s).
		// Classified separately so the executor can fail over to a
		// larger-context target.
		if strings.Contains(pe.Message, "exceeds the maximum number of tokens") {
			pe.Code = provcore.CodeContextOverflow
		}
	case "NOT_FOUND":
		pe.Code = provcore.CodeInvalidRequest
	case "UNAUTHENTICATED", "PERMISSION_DENIED":
		pe.Code = provcore.CodeAuthFailed
	case "RESOURCE_EXHAUSTED":
		// Deliberately NOT split into a quota-exhausted classification. Google
		// uses this one status for both a per-minute rate limit and a spent
		// project quota, and words both as "Quota exceeded for quota metric",
		// so there is no discriminator to read — not in the status, not in the
		// type, not in the message. Guessing would move a request off a
		// provider that a second of backoff would have served.
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

// IsStaleCacheRefError reports whether a Gemini 403 response body carries the
// stale-cachedContent error signature. Gemini phrases the message a few ways
// across API versions ("CachedContent not found", "permission denied" with the
// cache name, "GenerateContentRequest: cachedContent not found"); we match on
// the substrings that are stable across all of them, keeping false-positives
// low. Recognising Gemini's wire-error shape is a codec concern — the proxy
// only asks "is this the stale-cache signal?" and reacts (invalidate the
// cached ref); it never carries the Gemini prose itself.
func IsStaleCacheRefError(body []byte) bool {
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
