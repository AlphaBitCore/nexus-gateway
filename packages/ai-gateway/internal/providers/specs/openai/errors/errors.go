// Package errors implements the OpenAI-style error normalizer. It is an
// internal sub-package of specs/openai; the root package re-exports
// ErrorNormalizerInstance() via aliases.go for external callers.
package errors

import (
	"net/http"
	"strconv"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/tidwall/gjson"
)

// ErrorNormalizer maps OpenAI-style error envelopes
//
//	{"error":{"type":"...","message":"...","code":"..."}}
//
// onto canonical [provcore.ProviderError] codes. The mapping follows
// OpenAI's own documentation plus the common deviations seen in
// OpenAI-compatible upstreams (DeepSeek, GLM, Azure, Moonshot).
type ErrorNormalizer struct{}

// Normalize is exported implicitly via the AdapterSpec. The switch
// below is shared by every OpenAI-compat adapter; specs that need
// finer mapping (Anthropic, Gemini) define their own normalizer.
func (ErrorNormalizer) Normalize(status int, headers http.Header, body []byte) *provcore.ProviderError {
	pe := &provcore.ProviderError{
		Status: status,
		Raw:    body,
	}

	errObj := gjson.GetBytes(body, "error")
	if errObj.Exists() {
		pe.Type = errObj.Get("type").String()
		pe.Message = errObj.Get("message").String()
		if pe.Type == "" {
			pe.Type = errObj.Get("code").String()
		}
	}
	if pe.Message == "" {
		pe.Message = http.StatusText(status)
	}

	switch status {
	case http.StatusBadRequest:
		pe.Code = provcore.CodeInvalidRequest
	case http.StatusUnauthorized, http.StatusForbidden:
		pe.Code = provcore.CodeAuthFailed
	case http.StatusTooManyRequests:
		pe.Code = provcore.CodeRateLimited
		if ra := parseRetryAfter(headers.Get("Retry-After")); ra != nil {
			pe.RetryAfter = ra
		}
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		pe.Code = provcore.CodeTimeout
	case http.StatusNotFound:
		pe.Code = provcore.CodeInvalidRequest
	default:
		// Unrecognised error type from the upstream OpenAI-compatible API.
		// Both 5xx and non-5xx classify as CodeUpstreamError; finer
		// classification (e.g. CodeBadGateway on 5xx) would require
		// per-caller contract decisions.
		pe.Code = provcore.CodeUpstreamError
	}
	return pe
}

// parseRetryAfter honors both seconds ("17") and HTTP-date formats.
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
