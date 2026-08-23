// Package errors implements the OpenAI-style error normalizer. It is an
// internal sub-package of specs/openai; the root package re-exports
// ErrorNormalizerInstance() via aliases.go for external callers.
package errors

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
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
		// Context overflow rides the invalid_request envelope with a
		// dedicated code ("context_length_exceeded", documented) or the
		// message "This model's maximum context length is N tokens..."
		// (observed on gpt-4o 400s; OpenAI-compat upstreams reuse the
		// phrasing). Classified separately so the executor can fail over
		// to a larger-context target.
		if gjson.GetBytes(body, "error.code").String() == "context_length_exceeded" ||
			strings.Contains(pe.Message, "maximum context length") {
			pe.Code = provcore.CodeContextOverflow
		}
		// A spent account budget can also arrive as a 400 on OpenAI-compat
		// upstreams, which reuse the phrasing without the documented code.
		// Left as invalid_request it reads as the caller's fault, aborting the
		// request instead of moving it to a provider that can still serve it.
		if specutil.IsQuotaExhaustedMessage(pe.Message) {
			pe.Code = provcore.CodeProviderQuotaExhausted
		}
		pe.Message = appendResponsesAudioRemedy(pe.Message)
	case http.StatusUnauthorized, http.StatusForbidden:
		pe.Code = provcore.CodeAuthFailed
	case http.StatusTooManyRequests:
		pe.Code = provcore.CodeRateLimited
		// insufficient_quota is OpenAI's documented code for a spent account
		// budget and rides the same 429 as a rate limit. The two want opposite
		// handling: backing off and retrying clears a rate limit, and does
		// nothing at all for an account with no money in it — that one has to
		// move to another provider. A structured code is the discriminator
		// here rather than a message substring, because OpenAI publishes it.
		if gjson.GetBytes(body, "error.code").String() == "insufficient_quota" {
			pe.Code = provcore.CodeProviderQuotaExhausted
		}
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

// responsesAudioRejection is the verbatim opening of OpenAI's rejection when
// an audio content part is sent on the /v1/responses line. Observed 21 times
// in production on 2026-08-05, every row model gpt-audio-mini:
//
//	Invalid value: 'input_audio'. Supported values are: 'input_text',
//	'input_image', 'output_text', 'refusal', 'input_file',
//	'computer_screenshot', 'summary_text', and 'encrypted_content'.
const responsesAudioRejection = "Invalid value: 'input_audio'."

// appendResponsesAudioRemedy adds the one thing OpenAI's message cannot say.
//
// The message is already correct and specific: it names the rejected value
// and lists exactly what the line accepts. What it cannot know is that the
// SAME model serves audio on a different line — live-probed, gpt-audio-mini
// accepts input_audio on /v1/chat/completions and rejects it on
// /v1/responses. A caller reading only the upstream text concludes the model
// cannot take audio at all and gives up on a request the gateway could have
// served by routing elsewhere.
//
// Narrow on purpose. Only input_audio is evidenced, so only input_audio is
// matched: §3a Rule 7 forbids a rule that guesses at behaviour nobody
// observed. Other content parts absent from the Responses vocabulary
// (computer_screenshot is present, input_video does not exist anywhere) get
// no remedy until a real 400 shows one is needed.
//
// This does NOT intercept: the upstream's own text is preserved and the
// remedy is appended, so a caller who was reading the supported-values list
// still has it.
func appendResponsesAudioRemedy(msg string) string {
	if !strings.Contains(msg, responsesAudioRejection) {
		return msg
	}
	return msg + " Nexus: the /v1/responses line has no audio content part. " +
		"This model accepts audio on /v1/chat/completions — send the request there, " +
		"or route to a target whose wire carries audio."
}
