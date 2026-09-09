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
// through. Stamping approve here would make the stamp load-bearing, which is
// only true if the gate drops a zero-value action; keeping that rule in the gate
// is what stops the same knowledge from having to be remembered at each call
// site (it was not remembered in the shared emitter, and bodies went missing).
//
// The request body is deliberately NOT captured here — the parallel handlers'
// requests are multipart audio/video whose bytes are fingerprint-only;
// audio-playback capture is a separate concern with its own biometric
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
