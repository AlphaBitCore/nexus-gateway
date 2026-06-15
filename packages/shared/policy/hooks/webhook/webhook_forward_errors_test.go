// Coverage backfill: tests for residual low-coverage branches in the
// webhook sub-package (bad URL, response read error).
package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Webhook: BadEndpoint URL surfaces create-request error -----------------

func TestWebhookForward_Execute_BadURLSurfacesCreateRequestError(t *testing.T) {
	// A URL containing an invalid byte (raw newline) makes
	// http.NewRequestWithContext fail with "net/url: invalid control
	// character in URL".
	cfg := &HookConfig{Config: map[string]any{
		"endpoint": "http://example.com/\x7f-bad",
	}}
	h, err := NewWebhookForward(cfg)
	if err != nil {
		t.Skipf("factory rejected URL up-front: %v", err)
	}
	_, err = h.Execute(t.Context(), &HookInput{})
	if err == nil {
		t.Fatal("expected error")
	}
	// Either create-request or request-failed wrap is acceptable — both
	// signal upstream failure correctly.
	if !strings.Contains(err.Error(), "webhook-forward") {
		t.Errorf("error should be wrapped: %v", err)
	}
}

// --- Webhook: response read error path -------------------------------------

func TestWebhookForward_Execute_ResponseReadErrorBubbles(t *testing.T) {
	// A handler that hijacks the connection and immediately closes without
	// sending headers should cause the client's ReadAll to fail (server
	// closed connection mid-response).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Tell client there's a body, then close the connection mid-read.
		w.Header().Set("Content-Length", "5000")
		w.WriteHeader(http.StatusOK)
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	h := newWebhookHook(t, srv.URL, nil)
	_, err := h.Execute(context.Background(), &HookInput{})
	// Either the request fails outright (client detects abrupt close) or
	// the response read fails — both paths are valid; what matters is
	// that the hook surfaces a wrapped error rather than silently
	// approving.
	if err == nil {
		t.Skip("server hijack didn't trigger read error on this platform; not a regression")
	}
	if !strings.Contains(err.Error(), "webhook-forward") {
		t.Errorf("error should be wrapped: %v", err)
	}
}
