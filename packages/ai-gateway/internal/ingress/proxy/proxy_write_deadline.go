package proxy

import (
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

// extendWriteDeadlineToUpstreamBudget lifts the connection's write deadline to
// the upstream per-request budget (upstream.timeoutSec) before a proxy response
// is written.
//
// Why every proxy write path needs this: Go arms the server write deadline from
// server.writeTimeout when the request header is read, not when the handler
// writes. A non-streaming inference that spends minutes upstream is therefore
// already past that flat deadline by the time its body is written — the write
// fails and the client sees a severed connection instead of the answer it paid
// for. server.writeTimeout is sized for ordinary responses and must stay that
// way; the proxy paths opt out per-response instead.
//
// The four proxy write paths and how each is covered:
//   - streaming — stream_preamble.go arms the same budget and then wraps the
//     writer in streamIdleWriter, which re-arms StreamIdleTimeout on every
//     chunk. It sets the deadline inline because it needs the same
//     ResponseController for the wrapper.
//   - /v1/responses non-stream — proxy_responses.go arms it inline.
//   - general non-stream (chat/completions, /v1/messages, gemini, embeddings)
//     and the upstream-error write — both call this helper.
//
// SetWriteDeadline's error is deliberately discarded: a ResponseWriter that
// cannot carry a deadline (an unwrapped middleware wrapper, httptest) must
// still get its response written. Losing a completed inference because the
// deadline could not be extended would be worse than the truncation this
// prevents.
func extendWriteDeadlineToUpstreamBudget(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(specutil.ActiveConfig().Timeout))
}
