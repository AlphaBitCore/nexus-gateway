package tlsbump

import (
	"bytes"
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

// bodyLooksLikeEventStream reports whether an ALREADY-BUFFERED body is in fact
// an SSE event stream. It exists because the pre-read heuristic below cannot
// answer that question: chunked / no-Content-Length describes almost every
// dynamically generated JSON response, so it is true for ordinary traffic and
// carries no information about whether a stream was buffered by mistake.
// Measured on four ordinary non-stream chat completions: true on all four.
//
// Deciding after the read is definitive rather than heuristic — the bytes are
// already in hand, and this is the one condition worth an operator's attention
// on this path: a real event stream that isStreamingContentType did not
// recognise, so the client waited for the whole stream before seeing a byte.
//
// The check is line-anchored on SSE field names. A JSON body mentioning data or
// event carries them as quoted keys ("data":), never as a bare field at the
// start of a line, so the quote is what keeps this from firing on JSON.
func bodyLooksLikeEventStream(b []byte) bool {
	const window = 256 // an SSE stream declares itself in its first frame
	if len(b) > window {
		b = b[:window]
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimLeft(line, "\r \t")
		for _, field := range [][]byte{[]byte("data:"), []byte("event:"), []byte("retry:")} {
			if bytes.HasPrefix(line, field) {
				return true
			}
		}
	}
	return false
}

// looksLikeStreamingResponse is a heuristic for "this response is probably a
// stream even though its Content-Type wasn't recognised by
// isStreamingContentType": chunked transfer encoding or no fixed
// Content-Length. It is TRUE for ordinary JSON API responses, so it may only be
// used as context on a line that already fires for another reason — never as a
// trigger. bodyLooksLikeEventStream above is the one to use for a decision.
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

// requestCouldCarryContent reports whether a request may carry a body worth a
// compliance decision. Used to keep the uninspected-passthrough audit row to
// flows that could have contained a prompt or PII, rather than every asset
// fetch on an intercepted host.
//
// ContentLength < 0 means "unknown" (chunked), which counts: an unknown-length
// body is still a body.
func requestCouldCarryContent(r *http.Request) bool {
	if r == nil {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	}
	return r.ContentLength != 0
}
