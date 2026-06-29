package hooks

// Tests for hook_extras.go: RegisterHookExtrasRoutes, HookImplementations,
// HookExecutionChain, HookTest, forwardHookTest, runWebhookHookTest, trimRight,
// derefStr. All are currently at 0% coverage.

import (
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
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/hooks/hookstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

func TestTrimRight(t *testing.T) {
	tests := []struct {
		s, suffix, want string
	}{
		{"https://host/", "/", "https://host"},
		{"https://host///", "/", "https://host"},
		{"https://host", "/", "https://host"},
		{"", "/", ""},
		{"/", "/", ""},
	}
	for _, tc := range tests {
		got := trimRight(tc.s, tc.suffix)
		if got != tc.want {
			t.Errorf("trimRight(%q, %q) = %q; want %q", tc.s, tc.suffix, got, tc.want)
		}
	}
}

func TestDerefStr(t *testing.T) {
	if derefStr(nil) != "" {
		t.Errorf("nil should return empty string")
	}
	s := "hello"
	if derefStr(&s) != "hello" {
		t.Errorf("should deref to 'hello'")
	}
}

// RegisterHookExtrasRoutes — wires 4 endpoints

func TestRegisterHookExtrasRoutes_WiresFourEndpoints(t *testing.T) {
	h := newHandler(nil, nil, nil, audit.NewWriter(nil, "", slog.Default()))
	e := echo.New()
	g := e.Group("/api/admin")
	iamMW := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterHookExtrasRoutes(g, iamMW, ProxyConfig{AIGatewayURL: "http://localhost:3050"})

	wanted := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/admin/hooks/implementations"},
		{http.MethodGet, "/api/admin/hooks/execution-chain"},
		{http.MethodPost, "/api/admin/hooks/:id/test"},
		{http.MethodPost, "/api/admin/hooks/:id/dry-run"},
	}
	routes := e.Routes()
	for _, w := range wanted {
		found := false
		for _, r := range routes {
			if r.Method == w.method && r.Path == w.path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing route: %s %s", w.method, w.path)
		}
	}
	// Verify proxy was stored.
	if h.proxy.AIGatewayURL != "http://localhost:3050" {
		t.Errorf("proxy not stored; got %q", h.proxy.AIGatewayURL)
	}
}

func TestHookImplementations_ReturnsRegistryAndCategories(t *testing.T) {
	h := newHandler(nil, nil, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	if err := h.HookImplementations(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	data, ok := body["data"].([]any)
	if !ok || len(data) == 0 {
		t.Errorf("data missing or empty: %+v", body)
	}
	cats, ok := body["hookCategories"].([]any)
	if !ok || len(cats) == 0 {
		t.Errorf("hookCategories missing or empty: %+v", body)
	}
	// Every implementation listed in builtinHookImplementations must appear.
	if len(data) != len(builtinHookImplementations) {
		t.Errorf("data length = %d; want %d", len(data), len(builtinHookImplementations))
	}
}

func TestHookImplementations_AllImplsHaveRequiredFields(t *testing.T) {
	h := newHandler(nil, nil, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	_ = h.HookImplementations(c)

	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	for i, item := range body["data"].([]any) {
		impl, ok := item.(map[string]any)
		if !ok {
			t.Errorf("item %d is not a map", i)
			continue
		}
		if impl["implementationId"] == nil {
			t.Errorf("item %d missing implementationId", i)
		}
		if impl["hookType"] == nil {
			t.Errorf("item %d missing hookType", i)
		}
		if impl["supportedStages"] == nil {
			t.Errorf("item %d missing supportedStages", i)
		}
	}
}

func TestHookExecutionChain_EmptyHooks_ReturnsShell(t *testing.T) {
	mock, meta := newMockStore(t)
	// ListHookConfigs with no filter (q="", pipeline="") — only limit/offset passed.
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`SELECT .* FROM "HookConfig"`).
		WithArgs(1000, 0).
		WillReturnRows(pgxmock.NewRows(hookConfigCols))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")

	if err := h.HookExecutionChain(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body["totalHooks"] != float64(0) {
		t.Errorf("totalHooks = %v; want 0", body["totalHooks"])
	}
	if body["enabledHooks"] != float64(0) {
		t.Errorf("enabledHooks = %v; want 0", body["enabledHooks"])
	}
	flow, ok := body["flow"].([]any)
	if !ok || len(flow) != 5 {
		t.Errorf("flow should have 5 nodes; got %v", body["flow"])
	}
}

func TestHookExecutionChain_MixedHooks_RoutedCorrectly(t *testing.T) {
	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	category := "compliance"
	endpoint := "http://localhost:9000/pii"

	// Emit two hooks: one request-stage (enabled), one response-stage (disabled).
	row1 := []any{
		"hc-1", "pii-redact", "webhook", "pii-detector", "request", &category,
		&endpoint, (*string)(nil), json.RawMessage(`{"k":"v"}`), 10, 5000,
		"fail-open", true, []string{"openai"}, now, now,
	}
	row2 := []any{
		"hc-2", "quality", "builtin", "quality-checker", "response", (*string)(nil),
		(*string)(nil), (*string)(nil), json.RawMessage(nil), 20, 5000,
		"fail-open", false, []string(nil), now, now,
	}

	mock.ExpectQuery(`SELECT COUNT\(\*\)`).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(2))
	mock.ExpectQuery(`SELECT .* FROM "HookConfig"`).
		WithArgs(1000, 0).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(row1...).AddRow(row2...))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")

	if err := h.HookExecutionChain(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	if body["totalHooks"] != float64(2) {
		t.Errorf("totalHooks = %v; want 2", body["totalHooks"])
	}
	if body["enabledHooks"] != float64(1) {
		t.Errorf("enabledHooks = %v; want 1", body["enabledHooks"])
	}
	requestHooks, _ := body["requestHooks"].([]any)
	responseHooks, _ := body["responseHooks"].([]any)
	if len(requestHooks) != 1 {
		t.Errorf("requestHooks = %d; want 1", len(requestHooks))
	}
	if len(responseHooks) != 1 {
		t.Errorf("responseHooks = %d; want 1", len(responseHooks))
	}
}

func TestHookExecutionChain_DefaultStage_RoutesToRequest(t *testing.T) {
	// An unknown stage (not "request" or "response") falls through to requestHooks.
	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	row := []any{
		"hc-3", "custom", "builtin", "noop", "custom-stage", (*string)(nil),
		(*string)(nil), (*string)(nil), json.RawMessage(nil), 1, 5000,
		"fail-open", true, []string(nil), now, now,
	}
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`SELECT .* FROM "HookConfig"`).
		WithArgs(1000, 0).
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(row...))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	_ = h.HookExecutionChain(c)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	requestHooks, _ := body["requestHooks"].([]any)
	if len(requestHooks) != 1 {
		t.Errorf("non-canonical stage should route to requestHooks; got %d", len(requestHooks))
	}
}

// HookTest — not-found + webhook branch + builtin forward branch

func TestHookTest_NotFound_Returns404(t *testing.T) {
	mock, meta := newMockStore(t)
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("missing").
		WillReturnRows(pgxmock.NewRows(hookConfigCols))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.HookTest(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestHookTest_StoreError_Returns404(t *testing.T) {
	// GetHookConfig error results in (nil, err) → treated as not found.
	mock, meta := newMockStore(t)
	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("x").
		WillReturnError(io.ErrUnexpectedEOF)

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("x")
	if err := h.HookTest(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestHookTest_WebhookWithEndpoint_ForwardsToWebhookServer(t *testing.T) {
	// Stand up a tiny HTTP server to receive the webhook test.
	var receivedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"approved":true}`))
	}))
	defer srv.Close()

	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	endpoint := srv.URL
	row := makeHookConfigRow(now)
	// Override endpoint and type to be webhook.
	row[1] = "wh-hook"
	row[2] = "webhook"
	row[6] = &endpoint

	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("hc-1").
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(row...))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"test":"data"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("hc-1")

	if err := h.HookTest(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(receivedBody) == 0 {
		t.Error("webhook server should have received a body")
	}
	var respBody map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &respBody)
	if respBody["output"] == nil && respBody["error"] == nil {
		t.Errorf("response should have output or error; got %+v", respBody)
	}
}

func TestHookTest_BuiltinHook_ForwardsToAIGateway(t *testing.T) {
	// Stand up a tiny AI-Gateway-like server to receive the forward.
	var received []byte
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"passed":true}`))
	}))
	defer gw.Close()

	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	row := makeHookConfigRow(now)
	// Override to builtin type (no endpoint).
	row[2] = "builtin"

	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("hc-1").
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(row...))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	h.proxy = ProxyConfig{AIGatewayURL: gw.URL}

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("hc-1")

	if err := h.HookTest(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(received) == 0 {
		t.Error("AI gateway should have received forwarded payload")
	}
	// Payload must contain hookConfig.
	if !strings.Contains(string(received), "hookConfig") {
		t.Errorf("forwarded payload missing hookConfig; got %s", received)
	}
}

func TestHookTest_WebhookWithNoEndpoint_ForwardsToAIGateway(t *testing.T) {
	// A webhook hook with a nil endpoint should forward to AI gateway
	// (falls through to forwardHookTest).
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer gw.Close()

	mock, meta := newMockStore(t)
	now := time.Now().UTC()
	row := makeHookConfigRow(now)
	// Override type = webhook but endpoint = nil.
	row[2] = "webhook"
	row[6] = (*string)(nil)

	mock.ExpectQuery(`SELECT .* FROM "HookConfig" WHERE id`).
		WithArgs("hc-1").
		WillReturnRows(pgxmock.NewRows(hookConfigCols).AddRow(row...))

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	h.proxy = ProxyConfig{AIGatewayURL: gw.URL}

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")
	c.SetParamNames("id")
	c.SetParamValues("hc-1")

	if err := h.HookTest(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

// forwardHookTest — AI gateway unreachable branch

func TestForwardHookTest_AIGatewayUnreachable_ReturnsBadGateway(t *testing.T) {
	mock, meta := newMockStore(t)
	now := time.Now().UTC()

	h := newHandler(mock, meta, nil, audit.NewWriter(nil, "", slog.Default()))
	// Point at a port that refuses connections.
	h.proxy = ProxyConfig{AIGatewayURL: "http://127.0.0.1:1"}

	hc := &hookstore.HookConfig{
		ID:           "hc-1",
		Name:         "test",
		Type:         "builtin",
		Stage:        "request",
		TimeoutMs:    100,
		FailBehavior: "fail-open",
	}
	_ = now

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")

	err := h.forwardHookTest(c, hc)
	if err != nil {
		t.Fatalf("handler must not propagate transport errors; got: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unreachable") {
		t.Errorf("body should mention unreachable: %s", rec.Body.String())
	}
}

// runWebhookHookTest — transport error path

func TestRunWebhookHookTest_TransportError_ReturnsError(t *testing.T) {
	endpoint := "http://127.0.0.1:1/webhook" // no listener
	hc := &hookstore.HookConfig{
		ID:        "hc-1",
		Name:      "test",
		Type:      "webhook",
		Endpoint:  &endpoint,
		TimeoutMs: 100,
		Stage:     "request",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, elapsed, err := runWebhookHookTest(ctx, hc)
	if err == nil {
		t.Error("expected transport error; got nil")
	}
	if elapsed < 0 {
		t.Error("elapsed should be >= 0")
	}
}

func TestRunWebhookHookTest_ServerRespondsWith200(t *testing.T) {
	// Verify the happy path: server returns JSON, we parse and return it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	endpoint := srv.URL
	hc := &hookstore.HookConfig{
		ID:        "hc-1",
		Endpoint:  &endpoint,
		TimeoutMs: 5000,
		Stage:     "request",
	}
	output, elapsed, err := runWebhookHookTest(context.Background(), hc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if elapsed < 0 {
		t.Error("elapsed should be >= 0")
	}
	m, ok := output.(map[string]any)
	if !ok || m["status"] != "ok" {
		t.Errorf("output = %+v", output)
	}
}

// IAM catalog guard — extras routes use VerbRead consistently

func TestExtrasRouteVerbsKnownToCatalog(t *testing.T) {
	for _, v := range []iam.Verb{iam.VerbRead} {
		if !iam.ResourceHook.Allows(v) {
			t.Errorf("ResourceHook should allow verb %q", v)
		}
	}
}

// TestForwardHookTest_AttachesBearer verifies forwardHookTest carries
// Authorization: Bearer <token> on the CP→ai-gateway /internal/hooks-test
// call.
func TestForwardHookTest_AttachesBearer(t *testing.T) {
	const tok = "cp-internal-token"
	var gotAuth string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"passed":true}`))
	}))
	defer gw.Close()

	h := newHandler(nil, nil, nil, audit.NewWriter(nil, "", slog.Default()))
	h.proxy = ProxyConfig{AIGatewayURL: gw.URL, AIGatewayInternalToken: tok}

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u1")

	hc := &hookstore.HookConfig{TimeoutMs: 1000}
	if err := h.forwardHookTest(c, hc); err != nil {
		t.Fatalf("forwardHookTest: %v", err)
	}
	if want := "Bearer " + tok; gotAuth != want {
		t.Errorf("Authorization = %q; want %q", gotAuth, want)
	}
}
