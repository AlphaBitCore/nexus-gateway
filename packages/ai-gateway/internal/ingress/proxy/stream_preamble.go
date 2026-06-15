// stream_preamble.go — the preamble stage of the streaming stage
// chain: SSE response headers, the extended server write deadline, and
// the 200 flush. After this stage the HTTP status is committed — any
// later failure must be surfaced in-band (SSE error frame + audit
// classification), never as a different status code.
package proxy

import (
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
)

// streamPreambleStage writes the SSE response preamble.
type streamPreambleStage struct{ s *streamState }

func (st streamPreambleStage) run() bool {
	s := st.s
	w := s.w
	coerced := s.coerced

	// Match the upstream Anthropic / OpenAI Content-Type byte-for-byte
	// — both append `; charset=utf-8`.
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if len(coerced) > 0 {
		w.Header().Set("X-Nexus-Coerced", joinCSV(coerced))
	}

	// Streaming write deadline: idle-based, not a flat total cap. The
	// initial deadline uses the full upstream budget so a model that
	// thinks for minutes before the first token is not cut; once tokens
	// flow, each chunk write resets the deadline to StreamIdleTimeout
	// (see streamIdleWriter.Write), so an actively-producing stream runs
	// for any length and only a stalled upstream trips the deadline after
	// that much silence. The wrapped writer is published back onto the
	// stream state so the relay stage writes its SSE chunks through it.
	upstreamCfg := specutil.ActiveConfig()
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(upstreamCfg.Timeout))
		w = &streamIdleWriter{ResponseWriter: w, rc: rc, idle: upstreamCfg.StreamIdleTimeout}
		s.w = w
	}
	w.WriteHeader(http.StatusOK)
	return true
}
