package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// One x-request-id on the response, and it is the one the caller can correlate
// on: theirs when they sent one, the provider's when they did not.
//
// Both were emitted before. The RequestID middleware echoes a caller-supplied
// x-request-id, and this writer Added the upstream's afterwards, so a caller
// who sent the header got two values under one name — and every client
// library's Get returns the first. Measured against production: a request
// carrying "x-request-id: MINE-…" came back with MINE-… first and OpenAI's
// req_… second, invisible to anything that did not enumerate the values.
func TestWriteForwardedResponseHeaders_OneRequestIDAndItIsTheRightOne(t *testing.T) {
	const upstream = "req_from_the_provider"
	src := http.Header{"X-Request-Id": []string{upstream}}

	t.Run("the caller sent one, so it stands", func(t *testing.T) {
		w := httptest.NewRecorder()
		w.Header().Set("X-Request-Id", "MINE") // what the middleware stamps

		writeForwardedResponseHeaders(w, nil, provcore.FormatOpenAI, src, false)

		got := w.Header().Values("X-Request-Id")
		if len(got) != 1 {
			t.Fatalf("response carries %d values (%v); a client's Get reads the first, so a second hides the other", len(got), got)
		}
		if got[0] != "MINE" {
			t.Errorf("x-request-id = %q, want the caller's own MINE", got[0])
		}
	})

	t.Run("the caller sent none, so the provider's fills in", func(t *testing.T) {
		w := httptest.NewRecorder()

		writeForwardedResponseHeaders(w, nil, provcore.FormatOpenAI, src, false)

		got := w.Header().Values("X-Request-Id")
		if len(got) != 1 || got[0] != upstream {
			t.Errorf("x-request-id = %v, want exactly [%s] — with no caller id to preserve there is nothing to lose by surfacing the provider's", got, upstream)
		}
	})
}
