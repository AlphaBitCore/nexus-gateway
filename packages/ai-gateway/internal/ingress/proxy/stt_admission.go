// Request-admission gates for /v1/audio/transcriptions and /translations.
//
// Grouped here rather than trailing the handler because they answer one
// question between them — may this request proceed, and if not, with which
// status — and because a rejection has to be an explicit 400 rather than a
// silent unredacted egress. Splitting them also keeps stt_handler.go under
// the file-size ratchet, which is the repo's way of noticing a file has
// started collecting everything.

package proxy

import (
	"errors"
	"net/http"
	"strings"
)

// sttStreamRequested reports whether the client asked for a streamed
// transcription via the `stream` form field. Truthy = "true" / "1" (OpenAI
// sends "true"); anything else (absent, "false", "0") is non-streaming.
func sttStreamRequested(stream string) bool {
	switch strings.ToLower(strings.TrimSpace(stream)) {
	case "true", "1":
		return true
	default:
		return false
	}
}

// sttParseErrorResponse maps a ParseSTTMultipart error to the HTTP status,
// machine code, message, and hint the handler returns. A MaxBytesReader trip
// (the mid-stream size ceiling) is a 413 — enforced BEFORE the body drains, so
// a chunked / lying-Content-Length upload cannot force an unbounded read; every
// other malformed / governance-bound multipart error (missing file, missing
// model, duplicate governance field, multipart bomb) is a client 400 carrying
// the parser's own message.
func sttParseErrorResponse(err error) (status int, code, message, hint string) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return http.StatusRequestEntityTooLarge, "STT_UPLOAD_TOO_LARGE",
			"audio upload exceeds the STT size ceiling",
			"Reduce the audio file size, or an operator can raise AI_GATEWAY_GENERATIVE_CAP_STT_MAX_BYTES"
	}
	return http.StatusBadRequest, "STT_BAD_MULTIPART",
		err.Error(),
		"Fix the multipart body: exactly one file part, a model field, no duplicated model/response_format"
}

// sttFormatSupported reports whether a response_format is served in v1
// (json / verbose_json / text; empty = json default). srt / vtt / streaming
// are deferred → 400.
func sttFormatSupported(format string) bool {
	switch format {
	case "", "json", "verbose_json", "text":
		return true
	default:
		return false
	}
}
