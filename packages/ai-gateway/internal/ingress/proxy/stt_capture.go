package proxy

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/sttproxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
)

// captureSTTAudio records the transcribed audio on the audit row, under the
// same operator switch that governs every other request body.
//
// The bytes are HANDED OVER, not copied. They are already buffered for
// ReEmit, so the request-path cost is a slice assignment — measured at the
// 26 MiB ceiling, 3.3 ns and zero allocations, against 1.36 ms and 27 MB
// across 4 allocations for the pooled-copy path (and a buffer that large
// never returns to the pool, whose cap is 2 MiB). The audit writer spills
// anything past the inline ceiling, so the SIZE lands off-heap on the async
// side rather than on this path.
//
// Ownership passes to the record; nothing here writes to the slice
// afterwards, which is what lets the same bytes still be forwarded upstream.
// Capture must never change what goes on the wire, and sttproxy pins that
// with a test that re-emits the forward after capture has taken its
// reference.
//
// Without this the audio was unreachable after the fact: the transcription
// was auditable and the thing transcribed was not.
func (h *Handler) captureSTTAudio(rec *audit.Record, req *sttproxy.STTRequest) {
	if !h.payloadCaptureConfig().StoreRequestBody {
		return
	}
	audio, mime := req.Audio()
	if len(audio) == 0 {
		return
	}
	rec.AttachOwnedRequestBody(audio, mime)
}
