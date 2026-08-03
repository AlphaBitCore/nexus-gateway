package proxy

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
)

// captureParallelResponse stamps the response body of a parallel handler
// (ServeSTT / ServeVideoSubmit) into the audit record so the Traffic drawer's
// view-time normalize can render the transcript / job JSON, gated on the same
// payload-capture config the chat pipeline uses.
//
// ResponseAction is deliberately left unset. A parallel handler runs no response
// hook, so there is no redaction demand — which is precisely what an empty action
// means to redact.StorageRawBodyChecked, the one gate every service persists
// through. This used to stamp approve here and call the stamp load-bearing,
// because the gate did drop a zero-value action; moving that rule into the gate
// is what stopped the same knowledge from having to be remembered at each call
// site (it was not remembered in the shared emitter, and bodies went missing).
//
// The request body is deliberately NOT captured here — the parallel handlers'
// requests are multipart audio/video whose bytes are fingerprint-only (R-7);
// audio-playback capture is a separate concern (#29) with its own biometric
// governance.
func (h *Handler) captureParallelResponse(rec *audit.Record, body []byte, contentType string) {
	if rec == nil || len(body) == 0 {
		return
	}
	if !h.payloadCaptureConfig().StoreResponseBody {
		return
	}
	rec.ResponseBody = body
	if contentType != "" {
		rec.ResponseContentType = contentType
	}
}
