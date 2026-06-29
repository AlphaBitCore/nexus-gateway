package routing

import (
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// TestRoutingSimulate_ForwardsBodyAndStatus verifies the CP forwarder posts
// the bound body to the AG internal endpoint and returns the upstream status
// and payload verbatim.
func TestRoutingSimulate_ForwardsBodyAndStatus(t *testing.T) {
	var receivedPath, receivedBody string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		buf, _ := io.ReadAll(r.Body)
		receivedBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request":{"modelId":"gpt-4o-mini","endpointType":"chat/completions"},"originalModelId":"gpt-4o-mini","substituted":false,"stages":[],"trace":[],"targets":[],"recoveryTargets":[]}`))
	}))
	defer stub.Close()

	h := New(Deps{
		Proxy:  ProxyConfig{AIGatewayURL: stub.URL},
		Logger: slog.Default(),
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/routing-rules/simulate",
		strings.NewReader(`{"modelId":"gpt-4o-mini","endpointType":"chat"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.RoutingSimulate(c); err != nil {
		t.Fatalf("RoutingSimulate: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if receivedPath != "/internal/routing-simulate" {
		t.Errorf("forwarded path = %q; want /internal/routing-simulate", receivedPath)
	}
	if !strings.Contains(receivedBody, `"modelId":"gpt-4o-mini"`) || !strings.Contains(receivedBody, `"endpointType":"chat"`) {
		t.Errorf("upstream got %q; want it to preserve the client body", receivedBody)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp["originalModelId"] != "gpt-4o-mini" {
		t.Errorf("expected originalModelId passed through, got %v", resp["originalModelId"])
	}
}

// TestRoutingSimulate_StatusCodePassthrough verifies a non-2xx upstream
// status is mirrored back to the caller (e.g. 400 from AG for missing
// modelId).
func TestRoutingSimulate_StatusCodePassthrough(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"modelId is required"}`))
	}))
	defer stub.Close()

	h := New(Deps{
		Proxy:  ProxyConfig{AIGatewayURL: stub.URL},
		Logger: slog.Default(),
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/routing-rules/simulate",
		strings.NewReader(`{"modelId":"","endpointType":"chat"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.RoutingSimulate(c); err != nil {
		t.Fatalf("RoutingSimulate: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 passthrough", rec.Code)
	}
}

// TestRoutingSimulate_UpstreamUnreachable verifies a transport error returns
// 502 rather than 404 (404 is reserved for the endpoint itself being absent).
func TestRoutingSimulate_UpstreamUnreachable(t *testing.T) {
	h := New(Deps{
		// 127.0.0.1:1 is a port we're confident is closed.
		Proxy:  ProxyConfig{AIGatewayURL: "http://127.0.0.1:1"},
		Logger: slog.Default(),
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/routing-rules/simulate",
		strings.NewReader(`{"modelId":"x","endpointType":"chat"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.RoutingSimulate(c); err != nil {
		t.Fatalf("RoutingSimulate: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502 on transport error", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "AI Gateway unreachable") {
		t.Errorf("expected 'AI Gateway unreachable' in body; got %s", rec.Body.String())
	}
}

// TestRoutingSimulate_AttachesBearer verifies the routing-simulate forwarder
// carries Authorization: Bearer <token> on the CP→ai-gateway call.
func TestRoutingSimulate_AttachesBearer(t *testing.T) {
	const tok = "cp-internal-token"
	var gotAuth string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request":{},"originalModelId":"x","substituted":false,"stages":[],"trace":[],"targets":[],"recoveryTargets":[]}`))
	}))
	defer stub.Close()

	h := New(Deps{
		Proxy:  ProxyConfig{AIGatewayURL: stub.URL, AIGatewayInternalToken: tok},
		Logger: slog.Default(),
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/routing-rules/simulate",
		strings.NewReader(`{"modelId":"x","endpointType":"chat"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.RoutingSimulate(c); err != nil {
		t.Fatalf("RoutingSimulate: %v", err)
	}
	if want := "Bearer " + tok; gotAuth != want {
		t.Errorf("Authorization = %q; want %q", gotAuth, want)
	}
}
