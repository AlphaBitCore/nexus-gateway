package tlsbump

import (
	"net/http"
	"time"
)

// Relaying the upstream response to the client. Split out of forward_exchange.go, which owns the
// exchange's setup and its upstream call; this file owns what happens to the response on the way
// back. The seam is load-bearing for finding C-34: the stream-through arm's audit row is emitted by
// a defer AROUND this relay rather than before it, so the row carries the upstream timings the
// PhaseSink stamps off the body read.

// relayResponse copies the upstream response to the client, injecting the
// x-nexus-* marker headers via markerHook.
func (x *bumpedExchange) relayResponse(resp *http.Response) {
	if err := copyResponse(x.w, resp, markerHook(x.r.Context(), x.flow.bo.identity)); err != nil {
		ct := resp.Header.Get("Content-Type")
		// Rich diagnostic: a failed relay on a streaming endpoint is the
		// "we lost the chat reply" case. Record WHO canceled (client vs us),
		// the Content-Type + a streaming smell (to see whether a streaming
		// reply was mis-routed to this buffered/copy relay because its CT
		// isn't in isStreamingContentType), and timing — so the mechanism is
		// verifiable from agent.log alone. The audit ROW, when an audit context
		// exists, is emitted by runResponseStage — inline on the arms that buffered
		// the body, and via serveRequest's deferred call on the stream-through arm,
		// which must wait for this relay so the row carries the upstream timings
		// (finding C-34). Either way the row is written even if this copy fails,
		// because the deferred call is a defer. When no audit context exists
		// runResponseStage logged the UNAUDITED warning, so a failure here leaves a
		// paper trail on every path.
		x.flow.logger.Error("failed to copy upstream response",
			"target", x.flow.targetHost,
			"method", x.r.Method,
			"path", x.r.URL.Path,
			"status_code", resp.StatusCode,
			"content_type", ct,
			"is_sse", isStreamingContentType(ct),
			"maybe_buffered_stream", looksLikeStreamingResponse(resp),
			"cancel_cause", cancelCause(x.r.Context()),
			"duration_ms", int(time.Since(x.requestStart).Milliseconds()),
			"error", err,
		)
		// Response may be partially written; nothing more we can do.
	}
}
