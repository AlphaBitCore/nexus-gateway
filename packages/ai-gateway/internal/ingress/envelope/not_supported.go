package envelope

import (
	"net/http"
	"strings"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// WriteEndpointNotSupported answers an unmounted path with a JSON error
// envelope in the wire shape the caller's SDK expects.
//
// Go's http.ServeMux answers an unmatched pattern with `404 page not found` as
// text/plain. Both OpenAI SDKs JSON-parse every error body, so a text/plain 404
// surfaced as an APIStatusError carrying no message at all — the caller saw a
// 404 with nothing to explain it. Observed on staging 2026-07-27 for
// /v1/completions, /v1/moderations, /v1/images/edits and
// /v1/images/variations; found by sdk_compat/test_errors.py.
//
// The `type` is not_found_error and the `code` is the Nexus UPPER_SNAKE
// ENDPOINT_NOT_SUPPORTED, matching every other gateway-generated error. The
// message names the path so a caller reading only err.message can tell which
// call was wrong.
//
// The dialect is read from the path, because a miss has no body to read it
// from: the /v1/messages family gets the Anthropic envelope, the /v1beta
// family gets the Gemini one, and everything else gets the OpenAI shape. An
// SDK that only understands its own envelope would otherwise fail to parse
// this too, which is the same failure in a different costume.
func WriteEndpointNotSupported(w http.ResponseWriter, path string) {
	msg := "this gateway does not serve " + path
	pe := &provcore.ProviderError{
		Status:  http.StatusNotFound,
		Code:    "ENDPOINT_NOT_SUPPORTED",
		Message: msg,
	}
	format := notSupportedFormatForPath(path)
	writeInDialect(w, format, http.StatusNotFound, pe)
}

// notSupportedFormatForPath maps an unmounted path onto the dialect whose SDK
// most likely sent it. A caller that reached /v1beta is running the Gemini
// client; one that reached /v1/messages is running an Anthropic SDK. Anything
// else — including a bare typo at the root — gets the OpenAI shape, which is
// what the great majority of callers parse and what the rest of this gateway's
// own errors already use.
func notSupportedFormatForPath(path string) provcore.Format {
	switch {
	case strings.HasPrefix(path, "/v1/messages"), strings.HasPrefix(path, "/v1/complete"):
		return provcore.FormatAnthropic
	case strings.HasPrefix(path, "/v1beta"):
		return provcore.FormatGemini
	case strings.HasPrefix(path, "/v1/responses"):
		return provcore.FormatOpenAIResponses
	default:
		return provcore.FormatOpenAI
	}
}
