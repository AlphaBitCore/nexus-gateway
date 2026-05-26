package aigwsim

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
)

// nopProducer satisfies the mq.Producer interface with no-ops.
type nopProducer struct{}

func (n *nopProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (n *nopProducer) Enqueue(_ context.Context, _ string, _ []byte) error { return nil }
func (n *nopProducer) Close() error                                        { return nil }

func newTestHandler() *Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	aw := audit.NewWriter(&nopProducer{}, "audit", logger)
	return New(Deps{Audit: aw, Logger: logger})
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

func TestDefaultSimulatorTargetURL_Default(t *testing.T) {
	// Without env override, should return the fallback localhost URL.
	t.Setenv("AI_GATEWAY_URL", "")
	got := defaultSimulatorTargetURL()
	if got != "http://localhost:3050" {
		t.Errorf("got %q; want http://localhost:3050", got)
	}
}

func TestDefaultSimulatorTargetURL_EnvOverride(t *testing.T) {
	t.Setenv("AI_GATEWAY_URL", "https://ai-gw.example.com")
	got := defaultSimulatorTargetURL()
	if got != "https://ai-gw.example.com" {
		t.Errorf("got %q; want https://ai-gw.example.com", got)
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
				TargetURL: "http://localhost:3050",
				Path:      "/v1/models",
				Method:    "GET",
				VK:        "vk-test",
			},
			wantErr: false,
		},
		{
			name: "valid POST",
			req: simulatorForwardRequest{
				TargetURL: "https://ai.example.com",
				Path:      "/v1/chat/completions",
				Method:    "POST",
				VK:        "vk-test",
			},
			wantErr: false,
		},
		{
			name: "empty targetURL uses default",
			req: simulatorForwardRequest{
				TargetURL: "",
				Path:      "/v1/models",
				Method:    "GET",
				VK:        "vk-test",
			},
			wantErr: false,
		},
		{
			name: "bad scheme",
			req: simulatorForwardRequest{
				TargetURL: "ftp://bad.example.com",
				Path:      "/v1/models",
				Method:    "GET",
				VK:        "vk-test",
			},
			wantErr:     true,
			errContains: "not allowed",
		},
		{
			name: "no host",
			req: simulatorForwardRequest{
				TargetURL: "http://",
				Path:      "/v1/models",
				Method:    "GET",
				VK:        "vk-test",
			},
			wantErr:     true,
			errContains: "no host",
		},
		{
			name: "disallowed path",
			req: simulatorForwardRequest{
				TargetURL: "http://localhost:3050",
				Path:      "/v1/embeddings",
				Method:    "POST",
				VK:        "vk-test",
			},
			wantErr:     true,
			errContains: "allowlist",
		},
		{
			name: "disallowed method",
			req: simulatorForwardRequest{
				TargetURL: "http://localhost:3050",
				Path:      "/v1/models",
				Method:    "DELETE",
				VK:        "vk-test",
			},
			wantErr:     true,
			errContains: "not allowed",
		},
		{
			name: "missing vk",
			req: simulatorForwardRequest{
				TargetURL: "http://localhost:3050",
				Path:      "/v1/models",
				Method:    "GET",
				VK:        "",
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

// validateForwardRequest fills TargetURL with default when empty.
func TestValidateForwardRequest_FillsDefaultURL(t *testing.T) {
	t.Setenv("AI_GATEWAY_URL", "")
	req := simulatorForwardRequest{
		TargetURL: "",
		Path:      "/v1/models",
		Method:    "GET",
		VK:        "vk-abc",
	}
	if err := validateForwardRequest(&req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.TargetURL != "http://localhost:3050" {
		t.Errorf("TargetURL = %q; want http://localhost:3050", req.TargetURL)
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
		TargetURL: "http://localhost:3050",
		Path:      "/v1/models",
		Method:    "DELETE", // disallowed
		VK:        "vk-test",
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

	h := newTestHandler()
	e := echo.New()
	body, _ := json.Marshal(simulatorForwardRequest{
		TargetURL: upstream.URL,
		Path:      "/v1/models",
		Method:    "GET",
		VK:        "vk-test",
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
	// Use a port that is guaranteed not in use.
	h := newTestHandler()
	e := echo.New()
	body, _ := json.Marshal(simulatorForwardRequest{
		TargetURL: "http://127.0.0.1:19999",
		Path:      "/v1/models",
		Method:    "GET",
		VK:        "vk-test",
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

	h := newTestHandler()
	e := echo.New()
	chatBody := json.RawMessage(`{"model":"gpt-4","messages":[]}`)
	body, _ := json.Marshal(simulatorForwardRequest{
		TargetURL: upstream.URL,
		Path:      "/v1/chat/completions",
		Method:    "POST",
		VK:        "vk-test",
		Body:      chatBody,
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

	h := newTestHandler()
	e := echo.New()
	body, _ := json.Marshal(simulatorForwardRequest{
		TargetURL: upstream.URL,
		Path:      "/v1/chat/completions",
		Method:    "POST",
		VK:        "vk-test",
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

	h := newTestHandler()
	e := echo.New()
	body, _ := json.Marshal(simulatorForwardRequest{
		TargetURL: upstream.URL,
		Path:      "/v1/chat/completions",
		Method:    "POST",
		VK:        "vk-test",
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

func TestNew(t *testing.T) {
	h := newTestHandler()
	if h == nil {
		t.Fatal("New returned nil")
	}
	if h.logger == nil {
		t.Error("logger should be set")
	}
}

func TestNew_NilLogger_UsesDefault(t *testing.T) {
	h := New(Deps{Audit: nil, Logger: nil})
	if h == nil {
		t.Fatal("New returned nil")
	}
}
