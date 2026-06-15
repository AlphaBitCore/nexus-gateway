package tlsbump

import (
	"context"
	"net/http"
	"strings"
)

// Diagnostic classification helpers shared by the request/response forward
// path and its failure logs. Kept together so the routing decision and the
// failure diagnostics classify identically, and to keep the forward_* files
// under the size ratchet.

// cancelCause classifies why a request context ended, so failure logs can
// distinguish a CLIENT close (context.Canceled — the client gave up / raced
// another connection) from OUR own deadline (context.DeadlineExceeded) from a
// still-live context (the error came from elsewhere, e.g. the upstream reset).
func cancelCause(ctx context.Context) string {
	switch ctx.Err() {
	case context.Canceled:
		return "client_canceled"
	case context.DeadlineExceeded:
		return "our_deadline"
	default:
		return "none"
	}
}

// isStreamingContentType reports whether a response Content-Type must be
// relayed via the streaming path (handleSSEResponse) instead of buffered.
// SSE (text/event-stream) and Connect-RPC streaming
// (application/connect+proto|json) both stream the body chunk-by-chunk; if
// such a response is instead buffered (io.ReadAll), the client waits for the
// whole stream before seeing a byte and tends to cancel.
func isStreamingContentType(ct string) bool {
	return strings.Contains(ct, "text/event-stream") ||
		strings.Contains(ct, "application/connect+proto") ||
		strings.Contains(ct, "application/connect+json")
}

// looksLikeStreamingResponse is a heuristic for "this response is probably a
// stream even though its Content-Type wasn't recognised by
// isStreamingContentType": chunked transfer encoding or no fixed
// Content-Length. Used only for the diagnostic smell flag — never for
// routing — so a false positive is harmless.
func looksLikeStreamingResponse(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	for _, te := range resp.TransferEncoding {
		if te == "chunked" {
			return true
		}
	}
	return resp.ContentLength < 0
}

// responseRouteName labels the routing decision for the diagnostic log.
func responseRouteName(isSSE bool, audCtx *requestAuditCtx) string {
	switch {
	case isSSE:
		return "sse-stream"
	case audCtx == nil:
		return "unaudited-relay"
	default:
		return "buffered-or-fast"
	}
}

// responseArmName labels which non-SSE arm runResponseStage took.
func responseArmName(pErr error, needBuffer bool) string {
	switch {
	case pErr != nil:
		return "pipeline-build-error"
	case needBuffer:
		return "buffered-ai"
	default:
		return "stream-through-fast"
	}
}
