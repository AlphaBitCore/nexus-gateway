package tlsbump

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// stripDynamicHopByHop — RFC 7230 §6.1 dynamic Connection-listed strip

// TestSSE_StripsDynamicConnection verifies that headers named in the
// Connection header are removed by stripDynamicHopByHop, and that neither
// the dynamically-listed header nor Connection itself survive into the output
// (Connection is then removed by the static isHopByHopHeader sweep, but
// stripDynamicHopByHop alone is tested here in isolation).
func TestSSE_StripsDynamicConnection(t *testing.T) {
	h := http.Header{}
	// Simulate an upstream that lists X-Forwarded-For as a hop-by-hop header
	// via the Connection field.
	h.Set("Connection", "X-Forwarded-For")
	h.Set("X-Forwarded-For", "1.2.3.4")
	h.Set("Content-Type", "text/event-stream")

	stripDynamicHopByHop(h)

	// X-Forwarded-For must have been removed (it was listed in Connection).
	if got := h.Get("X-Forwarded-For"); got != "" {
		t.Errorf("X-Forwarded-For: want empty after strip, got %q", got)
	}

	// Content-Type must survive (not listed in Connection).
	if got := h.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type: want %q, got %q", "text/event-stream", got)
	}
}

// TestSSE_StripsDynamicConnection_MultipleNames verifies that a
// comma-separated Connection value strips all listed names.
func TestSSE_StripsDynamicConnection_MultipleNames(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "X-Forwarded-For, X-Real-Ip")
	h.Set("X-Forwarded-For", "1.2.3.4")
	h.Set("X-Real-Ip", "5.6.7.8")
	h.Set("Content-Type", "text/event-stream")

	stripDynamicHopByHop(h)

	if got := h.Get("X-Forwarded-For"); got != "" {
		t.Errorf("X-Forwarded-For: want empty, got %q", got)
	}
	if got := h.Get("X-Real-Ip"); got != "" {
		t.Errorf("X-Real-Ip: want empty, got %q", got)
	}
	if got := h.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type: want %q, got %q", "text/event-stream", got)
	}
}

// TestSSE_StripsDynamicConnection_MultipleConnectionLines verifies that
// multiple Connection header lines (unusual but valid per RFC 7230) are
// each processed.
func TestSSE_StripsDynamicConnection_MultipleConnectionLines(t *testing.T) {
	h := http.Header{}
	h.Add("Connection", "X-Forwarded-For")
	h.Add("Connection", "X-Custom-Hop")
	h.Set("X-Forwarded-For", "1.2.3.4")
	h.Set("X-Custom-Hop", "value")

	stripDynamicHopByHop(h)

	if got := h.Get("X-Forwarded-For"); got != "" {
		t.Errorf("X-Forwarded-For: want empty, got %q", got)
	}
	if got := h.Get("X-Custom-Hop"); got != "" {
		t.Errorf("X-Custom-Hop: want empty, got %q", got)
	}
}

// TestSSE_StripsDynamicConnection_EmptyConnection verifies that an absent
// Connection header is a no-op.
func TestSSE_StripsDynamicConnection_EmptyConnection(t *testing.T) {
	h := http.Header{}
	h.Set("X-Forwarded-For", "1.2.3.4")

	// Should not panic or remove anything.
	stripDynamicHopByHop(h)

	if got := h.Get("X-Forwarded-For"); got != "1.2.3.4" {
		t.Errorf("X-Forwarded-For: want %q, got %q", "1.2.3.4", got)
	}
}

// cpStampMarkers on SSE path — markers appear in response headers before flush

// TestSSEMarkers_FullMarker verifies that cpStampMarkers writes all
// x-nexus-cp-* headers onto w.Header() before WriteHeader is called,
// which is the SSE path's equivalent of cpMarkerHook on the non-streaming path.
func TestSSEMarkers_FullMarker(t *testing.T) {
	m := &CPMarker{
		RequestID:    "sse-req-1",
		DomainRuleID: "domain-uuid",
		HookOutcome:  traffic.HookOutcomeInput{Passed: []string{"pii-redact"}},
	}
	ctx := contextWithCPMarker(context.Background(), m)

	w := httptest.NewRecorder()
	stampMarkers(ctx, w.Header(), "compliance-proxy")
	w.WriteHeader(http.StatusOK)

	// Headers must appear on the recorder (before/at WriteHeader).
	h := w.Result().Header

	assertHeader(t, h, "X-Nexus-Via", "compliance-proxy")
	assertHeader(t, h, "X-Nexus-Hook", "passed:pii-redact")
	assertHeader(t, h, "X-Nexus-Domain-Rule", "domain-uuid")
	assertExposeContains(t, h,
		"X-Nexus-Hook", "X-Nexus-Domain-Rule")
}

// TestSSEMarkers_NilMarker verifies the minimal-mode fallback when no
// CPMarker was stashed on the context (e.g. compliance-disabled fast path).
func TestSSEMarkers_NilMarker(t *testing.T) {
	// No marker in context.
	ctx := context.Background()

	w := httptest.NewRecorder()
	stampMarkers(ctx, w.Header(), "compliance-proxy")
	w.WriteHeader(http.StatusOK)

	h := w.Result().Header

	assertHeader(t, h, "X-Nexus-Via", "compliance-proxy")

	if got := h.Get("X-Nexus-Domain-Rule"); got != "" {
		t.Errorf("X-Nexus-Domain-Rule: want empty, got %q", got)
	}
}

// TestSSEMarkers_NoDomainRule verifies that X-Nexus-Domain-Rule is absent
// when DomainRuleID is empty (passthrough traffic with no rule match).
func TestSSEMarkers_NoDomainRule(t *testing.T) {
	m := &CPMarker{
		RequestID:   "sse-req-2",
		HookOutcome: traffic.HookOutcomeInput{},
	}
	ctx := contextWithCPMarker(context.Background(), m)

	w := httptest.NewRecorder()
	stampMarkers(ctx, w.Header(), "compliance-proxy")
	w.WriteHeader(http.StatusOK)

	h := w.Result().Header

	assertHeader(t, h, "X-Nexus-Hook", "none")

	if got := h.Get("X-Nexus-Domain-Rule"); got != "" {
		t.Errorf("X-Nexus-Domain-Rule: want empty, got %q", got)
	}
}

// TestSSEMarkers_NotInBody verifies that marker headers are in the response
// header map (not the body), by asserting the recorder body does not contain
// the header names. This guards against accidentally writing markers as SSE
// event data instead of HTTP headers.
func TestSSEMarkers_NotInBody(t *testing.T) {
	m := &CPMarker{RequestID: "sse-req-3"}
	ctx := contextWithCPMarker(context.Background(), m)

	w := httptest.NewRecorder()
	stampMarkers(ctx, w.Header(), "compliance-proxy")
	w.WriteHeader(http.StatusOK)
	// Simulate two SSE events written after WriteHeader.
	_, _ = w.Write([]byte("data: hello\n\n"))
	_, _ = w.Write([]byte("data: world\n\n"))

	body := w.Body.String()

	// Marker names must not appear in the event body.
	for _, marker := range []string{"x-nexus-via", "x-nexus-hook", "x-nexus-domain-rule"} {
		if strings.Contains(strings.ToLower(body), marker) {
			t.Errorf("SSE body must not contain marker header name %q; body: %q", marker, body)
		}
	}

	// The two SSE events must be present in the body.
	if body != "data: hello\n\ndata: world\n\n" {
		t.Errorf("SSE body: want %q, got %q", "data: hello\n\ndata: world\n\n", body)
	}
}
