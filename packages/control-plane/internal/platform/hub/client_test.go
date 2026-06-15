package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCreateEnrollmentToken_success(t *testing.T) {
	wantExpires := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/enrollment/token" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("expected Bearer token, got %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":        "tok-id",
			"token":     "enroll-deadbeef",
			"thingType": "agent",
			"label":     "host",
			"expiresAt": wantExpires.Format(time.RFC3339Nano),
		})
	}))
	defer ts.Close()

	c := New(ts.URL, "test-token", ts.Client(), nil)
	out, err := c.CreateEnrollmentToken(context.Background(), CreateEnrollmentTokenRequest{
		ThingType: "agent",
		Label:     "host",
		CreatedBy: "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Token != "enroll-deadbeef" {
		t.Fatalf("token: got %q", out.Token)
	}
	if !out.ExpiresAt.Equal(wantExpires) {
		t.Fatalf("expiresAt: got %v want %v", out.ExpiresAt, wantExpires)
	}
}

func TestCreateEnrollmentToken_notConfigured(t *testing.T) {
	c := New("", "x", http.DefaultClient, nil)
	_, err := c.CreateEnrollmentToken(context.Background(), CreateEnrollmentTokenRequest{Label: "x", CreatedBy: "a"})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

func TestNotifyConfigChange_success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/config/update" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer svc-token" {
			t.Fatalf("bad auth: %q", r.Header.Get("Authorization"))
		}

		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if payload["thingType"] != "ai-gateway" {
			t.Fatalf("thingType: got %v", payload["thingType"])
		}
		if payload["configKey"] != "routing" {
			t.Fatalf("configKey: got %v", payload["configKey"])
		}
		if payload["action"] != "update" {
			t.Fatalf("action: got %v", payload["action"])
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":             true,
			"version":        5,
			"thingsNotified": 2,
			"thingsOnline":   3,
		})
	}))
	defer ts.Close()

	c := New(ts.URL, "svc-token", ts.Client(), nil)
	out, err := c.NotifyConfigChange(context.Background(), ConfigChangeRequest{
		ThingType: "ai-gateway",
		ConfigKey: "routing",
		State:     map[string]string{"default": "openai"},
		ActorID:   "user-1",
		ActorName: "Alice",
		SourceIP:  "10.0.0.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatal("expected ok=true")
	}
	if out.Version != 5 {
		t.Fatalf("version: got %d", out.Version)
	}
	if out.ThingsNotified != 2 {
		t.Fatalf("thingsNotified: got %d", out.ThingsNotified)
	}
	if out.ThingsOnline != 3 {
		t.Fatalf("thingsOnline: got %d", out.ThingsOnline)
	}
}

func TestNotifyConfigChange_defaultAction(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if payload["action"] != "update" {
			t.Fatalf("expected default action 'update', got %v", payload["action"])
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": 1, "thingsNotified": 0, "thingsOnline": 0})
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	_, err := c.NotifyConfigChange(context.Background(), ConfigChangeRequest{
		ThingType: "agent",
		ConfigKey: "hooks",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNotifyConfigChange_notConfigured(t *testing.T) {
	c := New("", "tok", nil, nil)
	_, err := c.NotifyConfigChange(context.Background(), ConfigChangeRequest{
		ThingType: "agent",
		ConfigKey: "hooks",
	})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

func TestNotifyConfigChange_retriesOnFailure(t *testing.T) {
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n <= 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("temporarily unavailable"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": 1, "thingsNotified": 0, "thingsOnline": 0})
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	out, err := c.NotifyConfigChange(context.Background(), ConfigChangeRequest{
		ThingType: "agent",
		ConfigKey: "hooks",
	})
	if err != nil {
		t.Fatalf("expected success on 4th attempt, got: %v", err)
	}
	if !out.OK {
		t.Fatal("expected ok=true")
	}
	if got := attempts.Load(); got != 4 {
		t.Fatalf("expected 4 attempts (1 initial + 3 retries), got %d", got)
	}
}

func TestNotifyConfigChange_exhaustsRetries(t *testing.T) {
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server error"))
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	_, err := c.NotifyConfigChange(context.Background(), ConfigChangeRequest{
		ThingType: "agent",
		ConfigKey: "hooks",
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if got := attempts.Load(); got != 4 {
		t.Fatalf("expected 4 attempts, got %d", got)
	}
}

// F-0108: a 4xx response (e.g. 422 on a malformed body) is deterministic — the
// identical retry will fail identically — so NotifyConfigChange must NOT retry
// it. Exactly one attempt is expected, and the error is returned immediately.
func TestNotifyConfigChange_noRetryOn422(t *testing.T) {
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"thingType and configKey are required"}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	_, err := c.NotifyConfigChange(context.Background(), ConfigChangeRequest{
		ThingType: "agent",
		ConfigKey: "hooks",
	})
	if err == nil {
		t.Fatal("expected error on 422")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 attempt on 422 (no retry); got %d", got)
	}
}

// F-0108: a 400 Bad Request is likewise non-retryable.
func TestNotifyConfigChange_noRetryOn400(t *testing.T) {
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	if _, err := c.NotifyConfigChange(context.Background(), ConfigChangeRequest{
		ThingType: "agent", ConfigKey: "hooks",
	}); err == nil {
		t.Fatal("expected error on 400")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 attempt on 400 (no retry); got %d", got)
	}
}

// F-0108: isRetryable classifies transport errors and 5xx as retryable, 4xx as
// not. Unit-level guard so the policy is pinned independent of the HTTP harness.
func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"network error", errors.New("connection refused"), true},
		{"500", &httpStatusError{status: 500}, true},
		{"503", &httpStatusError{status: 503}, true},
		{"400", &httpStatusError{status: 400}, false},
		{"409", &httpStatusError{status: 409}, false},
		{"422", &httpStatusError{status: 422}, false},
		{"499", &httpStatusError{status: 499}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryable(tc.err); got != tc.want {
				t.Errorf("isRetryable(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestInvalidateConfig_fireAndForget(t *testing.T) {
	var called atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if payload["state"] != nil {
			t.Fatalf("expected null state for Category B, got %v", payload["state"])
		}
		if payload["action"] != "update" {
			t.Fatalf("expected default action, got %v", payload["action"])
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": 2, "thingsNotified": 1, "thingsOnline": 1})
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	c.InvalidateConfig(context.Background(), "ai-gateway", "routing")

	if !called.Load() {
		t.Fatal("expected Hub to be called")
	}
}

func TestInvalidateConfig_notConfigured_noPanic(t *testing.T) {
	c := New("", "tok", nil, nil)
	c.InvalidateConfig(context.Background(), "agent", "hooks")
}

// TestInvalidateConfigE_success verifies the error-returning variant returns
// nil and reaches Hub on a successful push (F-0099 fail-loud path).
func TestInvalidateConfigE_success(t *testing.T) {
	var called atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": 3, "thingsNotified": 1, "thingsOnline": 1})
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	if err := c.InvalidateConfigE(context.Background(), "ai-gateway", "credentials"); err != nil {
		t.Fatalf("InvalidateConfigE: unexpected error %v", err)
	}
	if !called.Load() {
		t.Fatal("expected Hub to be called")
	}
}

// TestInvalidateConfigE_serverError_returnsErr is the core F-0099 guarantee:
// a failed push surfaces a non-nil error so the security-sensitive handler can
// return HTTP 502 instead of a false 2xx.
func TestInvalidateConfigE_serverError_returnsErr(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("hub crashed"))
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	if err := c.InvalidateConfigE(context.Background(), "ai-gateway", "virtual_keys"); err == nil {
		t.Fatal("InvalidateConfigE returned nil on Hub 500; want propagated error")
	}
}

// TestInvalidateConfigE_notConfigured_nil treats an unwired Hub (dev/local) as
// a no-op so a write does not 502 merely because Hub coordination is absent.
func TestInvalidateConfigE_notConfigured_nil(t *testing.T) {
	c := New("", "tok", nil, nil)
	if err := c.InvalidateConfigE(context.Background(), "ai-gateway", "providers"); err != nil {
		t.Fatalf("InvalidateConfigE on unconfigured Hub = %v; want nil (no-op)", err)
	}
}

// Extended tests

func TestInvalidateConfig_serverError_noReturn(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("crash"))
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	// InvalidateConfig is fire-and-forget: it should not panic on server error.
	c.InvalidateConfig(context.Background(), "agent", "hooks")
}

func TestInvalidateConfig_requestPayloadValidation(t *testing.T) {
	var (
		receivedPayload map[string]any
		mu              sync.Mutex
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(body, &receivedPayload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": 1, "thingsNotified": 0, "thingsOnline": 0})
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	c.InvalidateConfig(context.Background(), "compliance-proxy", "tls-rules")

	mu.Lock()
	defer mu.Unlock()
	if receivedPayload["thingType"] != "compliance-proxy" {
		t.Errorf("thingType = %v, want %q", receivedPayload["thingType"], "compliance-proxy")
	}
	if receivedPayload["configKey"] != "tls-rules" {
		t.Errorf("configKey = %v, want %q", receivedPayload["configKey"], "tls-rules")
	}
	if receivedPayload["state"] != nil {
		t.Errorf("state should be nil for Category B invalidation, got %v", receivedPayload["state"])
	}
}

func TestNotifyConfigChange_customAction(t *testing.T) {
	var receivedAction string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		receivedAction, _ = payload["action"].(string)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": 3, "thingsNotified": 1, "thingsOnline": 2})
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	out, err := c.NotifyConfigChange(context.Background(), ConfigChangeRequest{
		ThingType: "ai-gateway",
		ConfigKey: "routing",
		Action:    "delete",
		ActorID:   "u-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if receivedAction != "delete" {
		t.Errorf("action = %q, want %q", receivedAction, "delete")
	}
	if out.Version != 3 {
		t.Errorf("version = %d, want 3", out.Version)
	}
}

func TestNotifyConfigChange_contextCancelledDuringRetry(t *testing.T) {
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unavailable"))
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	c := New(ts.URL, "tok", ts.Client(), nil)
	_, err := c.NotifyConfigChange(ctx, ConfigChangeRequest{
		ThingType: "agent",
		ConfigKey: "hooks",
	})
	if err == nil {
		t.Fatal("expected error when context is cancelled during retries")
	}

	// Should have fewer than 4 attempts because context expired.
	if got := attempts.Load(); got >= 4 {
		t.Errorf("attempts = %d, expected fewer than 4 due to context cancellation", got)
	}
}

func TestNotifyConfigChange_invalidResponseBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{invalid json`))
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	_, err := c.NotifyConfigChange(context.Background(), ConfigChangeRequest{
		ThingType: "agent",
		ConfigKey: "hooks",
	})
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestBaseURL_and_Token(t *testing.T) {
	c := New("https://hub.nexus.internal:3060", "my-service-token", nil, nil)

	if got := c.BaseURL(); got != "https://hub.nexus.internal:3060" {
		t.Errorf("BaseURL() = %q, want %q", got, "https://hub.nexus.internal:3060")
	}
	if got := c.Token(); got != "my-service-token" {
		t.Errorf("Token() = %q, want %q", got, "my-service-token")
	}
}

func TestBaseURL_empty(t *testing.T) {
	c := New("", "tok", nil, nil)
	if got := c.BaseURL(); got != "" {
		t.Errorf("BaseURL() = %q, want empty string", got)
	}
}

func TestBaseURL_trailingSlash(t *testing.T) {
	c := New("https://hub.nexus.internal:3060/", "tok", nil, nil)
	if got := c.BaseURL(); got != "https://hub.nexus.internal:3060" {
		t.Errorf("BaseURL() = %q, want trailing slash stripped", got)
	}
}

func TestNew_nilHTTPClient_usesDefault(t *testing.T) {
	c := New("http://localhost", "tok", nil, nil)
	if c.BaseURL() != "http://localhost" {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL(), "http://localhost")
	}
}

func TestCreateEnrollmentToken_emptyServiceToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server should not be called when token is empty")
	}))
	defer ts.Close()

	c := New(ts.URL, "", ts.Client(), nil)
	_, err := c.CreateEnrollmentToken(context.Background(), CreateEnrollmentTokenRequest{
		Label:     "test",
		CreatedBy: "admin",
	})
	if err == nil {
		t.Fatal("expected error for empty service token")
	}
}

func TestCreateEnrollmentToken_emptyTokenInResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":     "",
			"expiresAt": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	_, err := c.CreateEnrollmentToken(context.Background(), CreateEnrollmentTokenRequest{
		Label:     "test",
		CreatedBy: "admin",
	})
	if err == nil {
		t.Fatal("expected error when hub returns empty token")
	}
}

func TestCreateEnrollmentToken_defaultThingType(t *testing.T) {
	var receivedType string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		receivedType, _ = payload["thingType"].(string)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":     "tok-123",
			"expiresAt": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	_, err := c.CreateEnrollmentToken(context.Background(), CreateEnrollmentTokenRequest{
		Label:     "test",
		CreatedBy: "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if receivedType != "agent" {
		t.Errorf("thingType = %q, want %q (default)", receivedType, "agent")
	}
}

func TestCreateEnrollmentToken_non201Status(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid thingType"))
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	_, err := c.CreateEnrollmentToken(context.Background(), CreateEnrollmentTokenRequest{
		ThingType: "agent",
		Label:     "x",
		CreatedBy: "a",
	})
	if err == nil {
		t.Fatal("expected error for 400 status")
	}
	// Confirm the response body is surfaced so admins can diagnose.
	if !strings.Contains(err.Error(), "status 400") || !strings.Contains(err.Error(), "invalid thingType") {
		t.Fatalf("error should include status + upstream body, got: %v", err)
	}
}

func TestCreateEnrollmentToken_invalidResponseBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	_, err := c.CreateEnrollmentToken(context.Background(), CreateEnrollmentTokenRequest{
		ThingType: "agent",
		Label:     "x",
		CreatedBy: "a",
	})
	if err == nil {
		t.Fatal("expected decode error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("error should wrap with decode-response context, got: %v", err)
	}
}

func TestCreateEnrollmentToken_networkError(t *testing.T) {
	// Spin up a server then immediately close it so Do() returns a connection error.
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close()

	c := New(url, "tok", &http.Client{Timeout: 500 * time.Millisecond}, nil)
	_, err := c.CreateEnrollmentToken(context.Background(), CreateEnrollmentTokenRequest{
		ThingType: "agent",
		Label:     "x",
		CreatedBy: "a",
	})
	if err == nil {
		t.Fatal("expected network error against closed server")
	}
	if !strings.Contains(err.Error(), "enrollment token request") {
		t.Fatalf("error should wrap with request context, got: %v", err)
	}
}

func TestNotifyConfigChange_emptyToken(t *testing.T) {
	c := New("http://hub.local", "", nil, nil)
	_, err := c.NotifyConfigChange(context.Background(), ConfigChangeRequest{
		ThingType: "agent",
		ConfigKey: "hooks",
	})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured for empty token, got %v", err)
	}
}

func TestDoConfigChange_networkError(t *testing.T) {
	// Start + close server to force connection refusal so doConfigChange's
	// httpClient.Do error path is exercised.
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close()

	c := New(url, "tok", &http.Client{Timeout: 200 * time.Millisecond}, nil)
	// Single attempt + retries each hit network err; final error wraps "config change failed after retries".
	_, err := c.NotifyConfigChange(context.Background(), ConfigChangeRequest{
		ThingType: "agent",
		ConfigKey: "hooks",
	})
	if err == nil {
		t.Fatal("expected error when Hub is unreachable")
	}
	if !strings.Contains(err.Error(), "config change failed after retries") {
		t.Fatalf("expected retry-wrapped error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "config change request") {
		t.Fatalf("expected inner request error, got: %v", err)
	}
}

func TestGetThingRuntime_success(t *testing.T) {
	wantBody := []byte(`{"snapshot":{"version":1},"meta":{"online":true}}`)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/things/thing-42/runtime" || r.Method != http.MethodGet {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer svc-token" {
			t.Fatalf("bad auth: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Fatalf("bad accept: %q", r.Header.Get("Accept"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(wantBody)
	}))
	defer ts.Close()

	c := New(ts.URL, "svc-token", ts.Client(), nil)
	body, status, err := c.GetThingRuntime(context.Background(), "thing-42")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if string(body) != string(wantBody) {
		t.Fatalf("body = %q, want %q", body, wantBody)
	}
}

func TestGetThingRuntime_passesThroughNon200(t *testing.T) {
	// GetThingRuntime is opaque — non-200 must be returned to caller with body
	// + status intact, not converted to an error.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"thing not found"}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	body, status, err := c.GetThingRuntime(context.Background(), "missing")
	if err != nil {
		t.Fatalf("expected nil err (pass-through), got: %v", err)
	}
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if !strings.Contains(string(body), "thing not found") {
		t.Fatalf("body lost: %q", body)
	}
}

func TestGetThingRuntime_notConfigured(t *testing.T) {
	c := New("", "tok", nil, nil)
	_, _, err := c.GetThingRuntime(context.Background(), "thing-1")
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

func TestGetThingRuntime_emptyToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("server should not be called when token is empty")
	}))
	defer ts.Close()

	c := New(ts.URL, "", ts.Client(), nil)
	_, _, err := c.GetThingRuntime(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected error for empty hub config token")
	}
	if !strings.Contains(err.Error(), "HUB_CONFIG_TOKEN") {
		t.Fatalf("error should mention the missing env var, got: %v", err)
	}
}

func TestGetThingRuntime_networkError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close()

	c := New(url, "tok", &http.Client{Timeout: 200 * time.Millisecond}, nil)
	_, _, err := c.GetThingRuntime(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected network error against closed server")
	}
}

func TestGetThingRuntime_badRequestURL(t *testing.T) {
	// Force NewRequestWithContext to fail by using a baseURL with an invalid scheme.
	// (The control-flow check `err != nil` after http.NewRequestWithContext is otherwise unreachable.)
	c := New("http://[::1", "tok", &http.Client{Timeout: 200 * time.Millisecond}, nil)
	_, _, err := c.GetThingRuntime(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected error for malformed baseURL")
	}
}

// readErrConn wraps an io.Reader and forces a read error on Body — used to
// simulate a truncated/aborted response so io.ReadAll fails. We achieve this
// in tests by using a server that hijacks the conn and closes it mid-stream
// after sending a Content-Length but no body. See TestGetThingRuntime_bodyReadError.

func TestGetThingRuntime_bodyReadError(t *testing.T) {
	// Server claims Content-Length but closes the conn before sending the body —
	// io.ReadAll then returns an unexpected-EOF error.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Skip("ResponseWriter not Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		// Send headers promising 100 bytes, then close immediately.
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n"))
		_ = conn.Close()
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", &http.Client{Timeout: 500 * time.Millisecond}, nil)
	_, status, err := c.GetThingRuntime(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected body-read error")
	}
	// Status should still be propagated even on body read failure (per impl).
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (propagated despite body error)", status)
	}
}

func TestGetThingServiceMeta_success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/things/thing-99/service-meta" || r.Method != http.MethodGet {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Fatalf("bad auth: %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ThingServiceMeta{
			ThingID:       "thing-99",
			ManagementURL: "https://gw.internal:3051",
		})
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	meta, err := c.GetThingServiceMeta(context.Background(), "thing-99")
	if err != nil {
		t.Fatal(err)
	}
	if meta.ThingID != "thing-99" {
		t.Fatalf("ThingID = %q, want thing-99", meta.ThingID)
	}
	if meta.ManagementURL != "https://gw.internal:3051" {
		t.Fatalf("ManagementURL = %q", meta.ManagementURL)
	}
}

func TestGetThingServiceMeta_notFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	_, err := c.GetThingServiceMeta(context.Background(), "ghost")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error should name the missing thing, got: %v", err)
	}
}

func TestGetThingServiceMeta_serverError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("db unreachable"))
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	_, err := c.GetThingServiceMeta(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected error for 500")
	}
	if !strings.Contains(err.Error(), "status 500") || !strings.Contains(err.Error(), "db unreachable") {
		t.Fatalf("error should include status + upstream body, got: %v", err)
	}
}

func TestGetThingServiceMeta_badJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{broken`))
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	_, err := c.GetThingServiceMeta(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode service-meta") {
		t.Fatalf("error should mention decode-service-meta, got: %v", err)
	}
}

func TestGetThingServiceMeta_notConfigured(t *testing.T) {
	c := New("", "tok", nil, nil)
	_, err := c.GetThingServiceMeta(context.Background(), "thing-1")
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

func TestGetThingServiceMeta_emptyToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("server should not be called when token is empty")
	}))
	defer ts.Close()

	c := New(ts.URL, "", ts.Client(), nil)
	_, err := c.GetThingServiceMeta(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestGetThingServiceMeta_networkError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close()

	c := New(url, "tok", &http.Client{Timeout: 200 * time.Millisecond}, nil)
	_, err := c.GetThingServiceMeta(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected network error")
	}
	if !strings.Contains(err.Error(), "service-meta request") {
		t.Fatalf("error should wrap with service-meta context, got: %v", err)
	}
}

func TestGetThingServiceMeta_badRequestURL(t *testing.T) {
	c := New("http://[::1", "tok", &http.Client{Timeout: 200 * time.Millisecond}, nil)
	_, err := c.GetThingServiceMeta(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected error for malformed baseURL")
	}
}

func TestGetThingServiceMeta_bodyReadError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n"))
		_ = conn.Close()
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", &http.Client{Timeout: 500 * time.Millisecond}, nil)
	_, err := c.GetThingServiceMeta(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected body-read error")
	}
	if !strings.Contains(err.Error(), "read service-meta body") {
		t.Fatalf("error should wrap with read-body context, got: %v", err)
	}
}

func TestForceResyncAll_success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/things/thing-7/resync" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer svc" {
			t.Fatalf("bad auth: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("bad content-type: %q", r.Header.Get("Content-Type"))
		}
		// Confirm the empty-object body is sent so Hub recognises this as "re-push all keys".
		body, _ := io.ReadAll(r.Body)
		if strings.TrimSpace(string(body)) != "{}" {
			t.Fatalf("body = %q, want %q", body, "{}")
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"pushed":  4,
			"thingId": "thing-7",
		})
	}))
	defer ts.Close()

	c := New(ts.URL, "svc", ts.Client(), nil)
	out, err := c.ForceResyncAll(context.Background(), "thing-7")
	if err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true {
		t.Fatalf("ok = %v, want true", out["ok"])
	}
	if out["thingId"] != "thing-7" {
		t.Fatalf("thingId = %v, want thing-7", out["thingId"])
	}
}

func TestForceResyncAll_notFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("missing"))
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	_, err := c.ForceResyncAll(context.Background(), "ghost")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error should name the missing thing, got: %v", err)
	}
}

func TestForceResyncAll_serverError(t *testing.T) {
	// Use 502 to exercise the generic >= 300 branch (not the special 404 branch).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream gone"))
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	_, err := c.ForceResyncAll(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected error for 502")
	}
	if !strings.Contains(err.Error(), "status 502") || !strings.Contains(err.Error(), "upstream gone") {
		t.Fatalf("error should include status + body, got: %v", err)
	}
}

func TestForceResyncAll_badJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[not an object]`))
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", ts.Client(), nil)
	_, err := c.ForceResyncAll(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode force-resync") {
		t.Fatalf("error should mention decode-force-resync, got: %v", err)
	}
}

func TestForceResyncAll_notConfigured(t *testing.T) {
	c := New("", "tok", nil, nil)
	_, err := c.ForceResyncAll(context.Background(), "thing-1")
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

func TestForceResyncAll_emptyToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("server should not be called when token is empty")
	}))
	defer ts.Close()

	c := New(ts.URL, "", ts.Client(), nil)
	_, err := c.ForceResyncAll(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestForceResyncAll_networkError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close()

	c := New(url, "tok", &http.Client{Timeout: 200 * time.Millisecond}, nil)
	_, err := c.ForceResyncAll(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected network error")
	}
	if !strings.Contains(err.Error(), "force-resync request") {
		t.Fatalf("error should wrap with force-resync context, got: %v", err)
	}
}

func TestForceResyncAll_badRequestURL(t *testing.T) {
	c := New("http://[::1", "tok", &http.Client{Timeout: 200 * time.Millisecond}, nil)
	_, err := c.ForceResyncAll(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected error for malformed baseURL")
	}
}

func TestForceResyncAll_bodyReadError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n"))
		_ = conn.Close()
	}))
	defer ts.Close()

	c := New(ts.URL, "tok", &http.Client{Timeout: 500 * time.Millisecond}, nil)
	_, err := c.ForceResyncAll(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("expected body-read error")
	}
	if !strings.Contains(err.Error(), "read force-resync body") {
		t.Fatalf("error should wrap with read-body context, got: %v", err)
	}
}

// ActorIdentity is a thin DTO — exercise the zero value to ensure the package
// stays buildable when callers omit the optional Email field.

func TestActorIdentity_zeroValue(t *testing.T) {
	a := ActorIdentity{}
	if a.ID != "" || a.Email != "" {
		t.Fatal("zero value should have empty ID + Email")
	}
}
