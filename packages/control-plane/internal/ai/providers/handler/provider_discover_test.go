// Coverage for provider_discover.go: RegisterProviderDiscoverRoutes,
// ProviderDiscoverModels, forwardDiscoverModels.
package providers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// RegisterProviderDiscoverRoutes — wires 1 route

func TestRegisterProviderDiscoverRoutes_WiresOneRoute(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	e := echo.New()
	g := e.Group("/api/admin")
	iamMW := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterProviderDiscoverRoutes(g, iamMW)
	want := map[string]bool{
		"POST /api/admin/providers/discover-models": false,
	}
	for _, r := range e.Routes() {
		if _, ok := want[r.Method+" "+r.Path]; ok {
			want[r.Method+" "+r.Path] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("route %s not registered", k)
		}
	}
}

// TestRegisterProviderDiscoverRoutes_DiscoverModelsGatedOnCreate proves that
// the discover-models route carries the same SSRF-vector reasoning as
// test-connection: the endpoint dials a caller-supplied base URL, so only a
// caller already authorized to configure a provider (provider:create) may run
// it. A read-only provider viewer cannot use it as a blind-SSRF oracle.
func TestRegisterProviderDiscoverRoutes_DiscoverModelsGatedOnCreate(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	e := echo.New()
	g := e.Group("/api/admin")

	var actions []string
	iamMW := func(action string) echo.MiddlewareFunc {
		actions = append(actions, action)
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterProviderDiscoverRoutes(g, iamMW)

	want := []string{"admin:provider.create"}
	if len(actions) != len(want) {
		t.Fatalf("iamMW invoked %d times %v; want %d %v", len(actions), actions, len(want), want)
	}
	if actions[0] != want[0] {
		t.Errorf("iamMW[0] = %q; want %q", actions[0], want[0])
	}
}

func TestProviderDiscoverModels_BadJSON_Returns400(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not-json"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.ProviderDiscoverModels(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
}

func TestProviderDiscoverModels_MissingRequired_Returns400(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"no baseUrl", `{"adapterType":"openai"}`},
		{"no adapterType", `{"baseUrl":"http://x"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			c, _ := echoCtx(req, rec, "u-1")
			_ = h.ProviderDiscoverModels(c)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: code = %d; want 400", tc.name, rec.Code)
			}
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body["error"] == nil {
				t.Errorf("%s: error field missing; body = %s", tc.name, rec.Body.String())
			}
		})
	}
}

func TestProviderDiscoverModels_InvalidAdapterType_Returns400(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	body := `{"adapterType":"INVALID","baseUrl":"http://x"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.ProviderDiscoverModels(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "adapterType must be one of") {
		t.Errorf("body should contain adapterType validation message; body = %s", rec.Body.String())
	}
}

func TestProviderDiscoverModels_AIGatewayUnreachable_ReturnsOK_SuccessFalse(t *testing.T) {
	// forwardDiscoverModels returns 200 even on transport failure (mirrors
	// forwardProviderTest: test-connection contract; the caller gets a
	// structured error body instead of an HTTP error code).
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{
		AIGatewayURL: "http://127.0.0.1:1", // no listener
	})
	body := `{"adapterType":"openai","baseUrl":"https://api.openai.com","apiKey":"sk-x"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.ProviderDiscoverModels(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200 (transport failures are 200+error body)", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["success"] != false {
		t.Errorf("success should be false on unreachable gateway; got %v", resp)
	}
	if _, ok := resp["error"]; !ok {
		t.Errorf("error field should be present; body = %s", rec.Body.String())
	}
}

func TestProviderDiscoverModels_AIGatewayResponds_PassesThrough(t *testing.T) {
	const gwBody = `{"success":true,"models":[{"id":"mock-gpt-4o-mini","suggestedType":"chat"}]}`
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the forwarded path.
		if r.URL.Path != "/internal/provider-discover-models" {
			t.Errorf("gateway received path %q; want /internal/provider-discover-models", r.URL.Path)
		}
		// Verify auth header is forwarded. In unit tests AIGatewayInternalToken
		// is empty, so the value is "Bearer " (with trailing space) — still a
		// valid Bearer prefix, confirming the header is set by the handler.
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer") {
			t.Errorf("Authorization header missing or malformed: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(gwBody))
	}))
	defer gw.Close()

	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{AIGatewayURL: gw.URL})
	body := `{"adapterType":"openai","baseUrl":"https://api.openai.com","apiKey":"sk-x"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.ProviderDiscoverModels(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["success"] != true {
		t.Errorf("success should be true; got %v", resp)
	}
	models, _ := resp["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("expected 1 model; got %d", len(models))
	}
	m, _ := models[0].(map[string]any)
	if m["id"] != "mock-gpt-4o-mini" {
		t.Errorf("model id = %q; want mock-gpt-4o-mini", m["id"])
	}
	if m["suggestedType"] != "chat" {
		t.Errorf("suggestedType = %q; want chat", m["suggestedType"])
	}
}

// forwardDiscoverModels — direct unit test

func TestForwardDiscoverModels_GatewayUnreachable_Returns200WithError(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{
		AIGatewayURL: "http://127.0.0.1:1",
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.forwardDiscoverModels(c, "openai", "http://api.example.com", "sk-x"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["success"] != false {
		t.Errorf("success should be false on unreachable gateway; got %v", body)
	}
	if _, ok := body["error"]; !ok {
		t.Errorf("error field should be present")
	}
}

// Compile-time guards: ensure imported packages are actually used.
var _ = time.Now
var _ = io.EOF
