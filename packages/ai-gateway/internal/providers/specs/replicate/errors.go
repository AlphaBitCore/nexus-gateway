package replicate

import (
	"net/http"
	"strconv"
	"time"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// errorNormalizer handles Replicate's error envelopes:
//
//	{"detail":"Authentication credentials were not provided."}
//	{"detail":"Invalid version or not permitted","title":"..."}
//	{"status":"failed","error":"<text>"}
type errorNormalizer struct{}

// Normalize implements provcore.ErrorNormalizer.
func (errorNormalizer) Normalize(status int, headers http.Header, body []byte) *provcore.ProviderError {
	pe := &provcore.ProviderError{Status: status, Raw: body}

	if msg := gjson.GetBytes(body, "detail"); msg.Type == gjson.String && msg.Str != "" {
		pe.Message = msg.Str
	} else if msg := gjson.GetBytes(body, "error"); msg.Type == gjson.String && msg.Str != "" {
		pe.Message = msg.Str
	} else if msg := gjson.GetBytes(body, "message"); msg.Type == gjson.String && msg.Str != "" {
		pe.Message = msg.Str
	}
	if pe.Message == "" {
		pe.Message = http.StatusText(status)
	}

	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		pe.Code = provcore.CodeInvalidRequest
	case http.StatusUnauthorized, http.StatusForbidden:
		pe.Code = provcore.CodeAuthFailed
	case http.StatusTooManyRequests:
		pe.Code = provcore.CodeRateLimited
		if ra := parseRetryAfter(headers.Get("retry-after")); ra != nil {
			pe.RetryAfter = ra
		}
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		pe.Code = provcore.CodeTimeout
	case http.StatusPaymentRequired:
		// Replicate returns 402 for quota / billing issues; map to
		// auth_failed which closest matches the canonical taxonomy.
		pe.Code = provcore.CodeAuthFailed
	default:
		pe.Code = provcore.CodeUpstreamError
	}
	return pe
}

func parseRetryAfter(v string) *time.Duration {
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
