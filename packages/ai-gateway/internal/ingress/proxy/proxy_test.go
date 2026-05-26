package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// TestBuildProviderRequest_ForwardsClientHeaders pins the wiring that
// lets per-format provider beta headers (anthropic-beta, openai-beta,
// x-goog-user-project, ...) reach the upstream. The spec adapter
// applies its own allowlist on top, so the proxy layer's contract is
// just "forward inbound headers verbatim".
//
// Regression for the context_management beta-header drop: pre-fix,
// fetchUpstream called provcore.Request{} without Headers, so even
// an `anthropic-beta` correctly stamped by the spec_anthropic
// PerFormatForwardHeaders allowlist had nothing to forward, and
// Anthropic rejected the request with "Extra inputs are not permitted".
func TestBuildProviderRequest_ForwardsClientHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("Anthropic-Beta", "context-management-2025-04-15")
	r.Header.Set("Anthropic-Version", "2023-06-01")
	r.Header.Set("Authorization", "Bearer should-be-stripped-by-adapter")

	in := Ingress{
		WireShape:   typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatAnthropic,
	}
	got := buildProviderRequest(r, in, []byte(`{"model":"claude-x"}`), false, 1<<20)

	if got.Headers.Get("Anthropic-Beta") != "context-management-2025-04-15" {
		t.Errorf("Anthropic-Beta: want forwarded, got %q", got.Headers.Get("Anthropic-Beta"))
	}
	if got.Headers.Get("Anthropic-Version") != "2023-06-01" {
		t.Errorf("Anthropic-Version: want forwarded, got %q", got.Headers.Get("Anthropic-Version"))
	}
	// Authorization passes through here — the allowlist filter that
	// strips it lives one layer down in spec_adapter.forwardHeaders.
	// What this test guarantees is that the proxy layer does NOT silently
	// drop ANY header before the adapter has a chance to apply its own
	// per-format rules.
	if got.Headers.Get("Authorization") != "Bearer should-be-stripped-by-adapter" {
		t.Errorf("Authorization: want forwarded to adapter for filtering, got %q",
			got.Headers.Get("Authorization"))
	}
	if got.MaxResponseBytes != 1<<20 {
		t.Errorf("MaxResponseBytes: want 1MiB, got %d", got.MaxResponseBytes)
	}
	if got.BodyFormat != provcore.FormatAnthropic {
		t.Errorf("BodyFormat: want anthropic, got %s", got.BodyFormat)
	}
}

// TestBuildProviderRequest_NilRequest guards against a nil http.Request
// so callers can construct a Request defensively (e.g. internal
// retries) without panicking.
func TestBuildProviderRequest_NilRequest(t *testing.T) {
	in := Ingress{
		WireShape:   typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	}
	got := buildProviderRequest(nil, in, nil, false, 0)
	if got.Headers != nil {
		t.Errorf("Headers from nil request: want nil, got %v", got.Headers)
	}
}

func TestWriteJSONError(t *testing.T) {
	// Basic smoke test — just ensure it doesn't panic.
	rec := &testResponseWriter{}
	writeJSONError(rec, 400, "bad request")
	if rec.status != 400 {
		t.Errorf("status = %d, want 400", rec.status)
	}
	if rec.header.Get("Content-Type") != "application/json" {
		t.Error("Content-Type not set")
	}
}

type testResponseWriter struct {
	header http.Header
	status int
	body   []byte
}

func (w *testResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *testResponseWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return len(b), nil
}

func (w *testResponseWriter) WriteHeader(status int) {
	w.status = status
}
