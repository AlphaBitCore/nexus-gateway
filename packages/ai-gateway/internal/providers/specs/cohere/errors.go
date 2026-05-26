package cohere

import (
	"net/http"
	"strconv"
	"time"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// errorNormalizer handles Cohere's error envelopes:
//
//	{"message":"<text>"}
//	{"data":"<text>","status":401}
//	{"error":{"message":"<text>","type":"..."}}
type errorNormalizer struct{}

// Normalize implements provcore.ErrorNormalizer.
func (errorNormalizer) Normalize(status int, headers http.Header, body []byte) *provcore.ProviderError {
	pe := &provcore.ProviderError{Status: status, Raw: body}

	if msg := gjson.GetBytes(body, "message"); msg.Type == gjson.String && msg.Str != "" {
		pe.Message = msg.Str
	} else if msg := gjson.GetBytes(body, "error.message"); msg.Type == gjson.String && msg.Str != "" {
		pe.Message = msg.Str
		pe.Type = gjson.GetBytes(body, "error.type").Str
	} else if msg := gjson.GetBytes(body, "data"); msg.Type == gjson.String && msg.Str != "" {
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
