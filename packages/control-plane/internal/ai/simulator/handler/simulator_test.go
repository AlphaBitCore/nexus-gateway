package aigwsim

import (
	"bytes"
	"context"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

func newTestHandler() *Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(Deps{Logger: logger})
}

func TestErrJSON(t *testing.T) {
	got := errJSON("msg", "bad_type", "C001")
	inner, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatal("expected nested error map")
	}
	if inner["message"] != "msg" || inner["type"] != "bad_type" || inner["code"] != "C001" {
		t.Errorf("unexpected inner map: %v", inner)
	}
}

func TestInternalServerError(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	err := internalServerError(c, "boom")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestConfiguredGatewayURL_Default(t *testing.T) {
	// Without env override, should return the fallback localhost URL.
	t.Setenv("AI_GATEWAY_URL", "")
	got, err := configuredGatewayURL()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "http://localhost:3050" {
		t.Errorf("got %q; want http://localhost:3050", got)
	}
}

func TestConfiguredGatewayURL_EnvOverride(t *testing.T) {
	t.Setenv("AI_GATEWAY_URL", "https://ai-gw.example.com/")
	got, err := configuredGatewayURL()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "https://ai-gw.example.com" { // trailing slash trimmed
		t.Errorf("got %q; want https://ai-gw.example.com", got)
	}
}

func TestConfiguredGatewayURL_Misconfigured(t *testing.T) {
	for _, tc := range []struct{ name, val string }{
		{"bad scheme", "ftp://ai-gw.example.com"},
		{"no host", "http://"},
		{"parse error", "http://[::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AI_GATEWAY_URL", tc.val)
			if _, err := configuredGatewayURL(); err == nil {
				t.Errorf("expected error for %q, got nil", tc.val)
			}
		})
	}
}

func TestIsAllowedSimulatorPath(t *testing.T) {
	tests := []struct {
		path  string
		allow bool
	}{
		{"", false},
		{"/v1/models", true},
		{"/v1/chat/completions", true},
		{"/v1/messages", true},
		{"/v1/usage", true},
		// Gemini paths
		{"/v1beta/models/gemini-pro:generateContent", true},
		{"/v1beta/models/gemini-pro:streamGenerateContent", true},
		// Gemini invalid suffix
		{"/v1beta/models/gemini-pro:unknownAction", false},
		// Path traversal
		{"/v1/../etc/passwd", false},
		// Query string
		{"/v1/models?foo=bar", false},
		// Fragment
		{"/v1/models#anchor", false},
		// Not in allowlist
		{"/v1/embeddings", false},
		{"/v1/completions", false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got := isAllowedSimulatorPath(tc.path)
			if got != tc.allow {
				t.Errorf("isAllowedSimulatorPath(%q) = %v; want %v", tc.path, got, tc.allow)
			}
		})
	}
}

func TestValidateForwardRequest(t *testing.T) {
	tests := []struct {
		name        string
		req         simulatorForwardRequest
		wantErr     bool
		errContains string
	}{
		{
			name: "valid GET",
			req: simulatorForwardRequest{
				Path:   "/v1/models",
				Method: "GET",
				VK:     "vk-test",
			},
			wantErr: false,
		},
		{
			name: "valid POST",
			req: simulatorForwardRequest{
				Path:   "/v1/chat/completions",
				Method: "POST",
				VK:     "vk-test",
			},
			wantErr: false,
		},
		{
			name: "disallowed path",
			req: simulatorForwardRequest{
				Path:   "/v1/embeddings",
				Method: "POST",
				VK:     "vk-test",
			},
			wantErr:     true,
			errContains: "allowlist",
		},
		{
			name: "disallowed method",
			req: simulatorForwardRequest{
				Path:   "/v1/models",
				Method: "DELETE",
				VK:     "vk-test",
			},
			wantErr:     true,
			errContains: "not allowed",
		},
		{
			name: "missing vk",
			req: simulatorForwardRequest{
				Path:   "/v1/models",
				Method: "GET",
				VK:     "",
			},
			wantErr:     true,
			errContains: "vk is required",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			err := validateForwardRequest(&req)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errContains)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestAIGatewaySimulatorForward_IgnoresCallerSuppliedHost verifies that a
// caller that smuggles a targetUrl into the JSON body must NOT be
// able to redirect the proxy — the request always goes to the configured
// gateway. We stand up two servers; only the configured one must be hit.
func TestAIGatewaySimulatorForward_IgnoresCallerSuppliedHost(t *testing.T) {
	configuredHit := false
	configured := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		configuredHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer configured.Close()

	attackerHit := false
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	t.Setenv("AI_GATEWAY_URL", configured.URL)
	h := newTestHandler()
	e := echo.New()
	// Raw JSON carrying an unknown "targetUrl" field pointing at the attacker
	// server — the struct no longer has the field, so it must be ignored.
	raw := `{"targetUrl":"` + attacker.URL + `","path":"/v1/models","method":"GET","vk":"vk-test"}`
	req := httptest.NewRequest(http.MethodPost, "/forward", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.AIGatewaySimulatorForward(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attackerHit {
		t.Error("attacker-supplied host was reached — host pinning failed (F-0269)")
	}
	if !configuredHit {
		t.Error("configured gateway was not reached")
	}
}

// AIGatewaySimulatorForward — misconfigured gateway URL → 500 (not 400)

func TestAIGatewaySimulatorForward_MisconfiguredGateway_Returns500(t *testing.T) {
	t.Setenv("AI_GATEWAY_URL", "ftp://bad.example.com") // invalid scheme
	h := newTestHandler()
	e := echo.New()
	body, _ := json.Marshal(simulatorForwardRequest{
		Path:   "/v1/models",
		Method: "GET",
		VK:     "vk-test",
	})
	req := httptest.NewRequest(http.MethodPost, "/forward", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.AIGatewaySimulatorForward(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500 (operator misconfig, not caller error)", rec.Code)
	}
}

// AIGatewaySimulatorForward — bind failure

func TestAIGatewaySimulatorForward_BadBody(t *testing.T) {
	h := newTestHandler()
	e := echo.New()
	// Send non-JSON body with Content-Type application/json — Bind will fail.
	req := httptest.NewRequest(http.MethodPost, "/forward", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.AIGatewaySimulatorForward(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

// AIGatewaySimulatorForward — validation failure

func TestAIGatewaySimulatorForward_ValidationFailure(t *testing.T) {
	h := newTestHandler()
	e := echo.New()
	body, _ := json.Marshal(simulatorForwardRequest{
		Path:   "/v1/models",
		Method: "DELETE", // disallowed
		VK:     "vk-test",
	})
	req := httptest.NewRequest(http.MethodPost, "/forward", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.AIGatewaySimulatorForward(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

// AIGatewaySimulatorForward — upstream success

func TestAIGatewaySimulatorForward_UpstreamSuccess(t *testing.T) {
	// Stand up a local fake AI-gateway.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"object":"list","data":[]}`)) //nolint:errcheck
	}))
	defer upstream.Close()
	t.Setenv("AI_GATEWAY_URL", upstream.URL)

	h := newTestHandler()
	e := echo.New()
	body, _ := json.Marshal(simulatorForwardRequest{
		Path:   "/v1/models",
		Method: "GET",
		VK:     "vk-test",
	})
	req := httptest.NewRequest(http.MethodPost, "/forward", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.AIGatewaySimulatorForward(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

// AIGatewaySimulatorForward — upstream down (connection refused)

func TestAIGatewaySimulatorForward_UpstreamDown(t *testing.T) {
	// Point the configured gateway at a port that is guaranteed not in use.
	t.Setenv("AI_GATEWAY_URL", "http://127.0.0.1:19999")
	h := newTestHandler()
	e := echo.New()
	body, _ := json.Marshal(simulatorForwardRequest{
		Path:   "/v1/models",
		Method: "GET",
		VK:     "vk-test",
	})
	req := httptest.NewRequest(http.MethodPost, "/forward", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.AIGatewaySimulatorForward(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", rec.Code)
	}
}

// AIGatewaySimulatorForward — upstream POST with body

func TestAIGatewaySimulatorForward_PostWithBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		data, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(data) //nolint:errcheck
	}))
	defer upstream.Close()
	t.Setenv("AI_GATEWAY_URL", upstream.URL)

	h := newTestHandler()
	e := echo.New()
	chatBody := json.RawMessage(`{"model":"gpt-4","messages":[]}`)
	body, _ := json.Marshal(simulatorForwardRequest{
		Path:   "/v1/chat/completions",
		Method: "POST",
		VK:     "vk-test",
		Body:   chatBody,
	})
	req := httptest.NewRequest(http.MethodPost, "/forward", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.AIGatewaySimulatorForward(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestSimulatorFlushWriter_Write(t *testing.T) {
	rec := httptest.NewRecorder()
	rc := http.NewResponseController(rec)
	fw := &simulatorFlushWriter{w: rec, rc: rc}
	n, err := fw.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 5 {
		t.Errorf("n = %d; want 5", n)
	}
	if rec.Body.String() != "hello" {
		t.Errorf("body = %q; want hello", rec.Body.String())
	}
}

// AIGatewaySimulatorForward — Accept header pass-through

func TestAIGatewaySimulatorForward_AcceptHeaderPassThrough(t *testing.T) {
	var receivedAccept string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	t.Setenv("AI_GATEWAY_URL", upstream.URL)

	h := newTestHandler()
	e := echo.New()
	body, _ := json.Marshal(simulatorForwardRequest{
		Path:   "/v1/chat/completions",
		Method: "POST",
		VK:     "vk-test",
	})
	req := httptest.NewRequest(http.MethodPost, "/forward", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.AIGatewaySimulatorForward(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedAccept != "text/event-stream" {
		t.Errorf("upstream received Accept=%q; want text/event-stream", receivedAccept)
	}
}

// AIGatewaySimulatorForward — canceled context returns 499

func TestAIGatewaySimulatorForward_CanceledContext_Returns499(t *testing.T) {
	// The upstream blocks until the context is canceled.
	ready := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(ready)
		// Block until client disconnects.
		<-r.Context().Done()
	}))
	defer upstream.Close()
	t.Setenv("AI_GATEWAY_URL", upstream.URL)

	h := newTestHandler()
	e := echo.New()
	body, _ := json.Marshal(simulatorForwardRequest{
		Path:   "/v1/chat/completions",
		Method: "POST",
		VK:     "vk-test",
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/forward", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.AIGatewaySimulatorForward(c) //nolint:errcheck
	}()

	// Wait for upstream to receive the request, then cancel.
	<-ready
	cancel()
	<-done

	if rec.Code != 499 {
		t.Errorf("status = %d; want 499", rec.Code)
	}
}

// New constructor

// TestNewSimulatorForwardClient_Timeout verifies the forward client applies a
// real ResponseHeaderTimeout (= simulatorForwardTimeout) so a wedged upstream
// that never sends headers cannot pin the admin request forever, while keeping
// Client.Timeout == 0 so an in-progress streaming response is not truncated
// (F-0272c).
func TestNewSimulatorForwardClient_Timeout(t *testing.T) {
	client := newSimulatorForwardClient()

	if client.Timeout != 0 {
		t.Errorf("Client.Timeout = %v; want 0 (streaming must not be capped by the client deadline)", client.Timeout)
	}

	type unwrapper interface{ Unwrap() http.RoundTripper }
	uw, ok := client.Transport.(unwrapper)
	if !ok {
		t.Fatalf("transport %T does not expose Unwrap()", client.Transport)
	}
	tr, ok := uw.Unwrap().(*http.Transport)
	if !ok {
		t.Fatalf("unwrapped transport is %T; want *http.Transport", uw.Unwrap())
	}
	if tr.ResponseHeaderTimeout != simulatorForwardTimeout {
		t.Errorf("ResponseHeaderTimeout = %v; want %v", tr.ResponseHeaderTimeout, simulatorForwardTimeout)
	}
	if simulatorForwardTimeout != 120*time.Second {
		t.Errorf("simulatorForwardTimeout = %v; want 120s", simulatorForwardTimeout)
	}
}

func TestNew(t *testing.T) {
	h := newTestHandler()
	if h == nil {
		t.Fatal("New returned nil")
		return
	}
	if h.logger == nil {
		t.Error("logger should be set")
	}
}

func TestNew_NilLogger_UsesDefault(t *testing.T) {
	h := New(Deps{Logger: nil})
	if h == nil {
		t.Fatal("New returned nil")
	}
}
