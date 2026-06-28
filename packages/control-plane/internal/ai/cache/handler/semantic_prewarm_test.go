// Package cache: semantic_prewarm_test.go — unit tests for the
// SemanticCacheHandler.PrewarmCache endpoint.
//
// All tests use httptest.Server to mock the AI Gateway internal endpoint
// and Echo's httptest pattern for the CP handler — no live DB or
// AI Gateway required.
package cache_test

import (
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	cache "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/cache/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

// buildPrewarmHandler creates a SemanticCacheHandler wired to the given
// AI Gateway mock server URL. store may be nil.
func buildPrewarmHandler(t *testing.T, aiGWURL string, store cache.SemanticCacheStore) *cache.SemanticCacheHandler {
	t.Helper()
	return cache.NewSemanticCacheHandler(cache.SemanticCacheHandlerDeps{
		Store:        store,
		AIGatewayURL: aiGWURL,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// mockAIGW starts a test HTTP server that responds to
// POST /internal/semantic-prewarm with the supplied handler.
func mockAIGW(t *testing.T, fn http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/semantic-prewarm", fn)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// singlePrewarmEntry returns a minimal valid prewarm request body JSON.
func singlePrewarmEntry() string {
	return `{"entries":[{"prompt":"What is Go?","response":"Go is a compiled language.","model":"gpt-4o","ttlSeconds":3600}],"dryRun":false}`
}

// threePrewarmEntries returns a valid 3-entry corpus body.
func threePrewarmEntries(dryRun bool) string {
	dr := "false"
	if dryRun {
		dr = "true"
	}
	return `{"entries":[` +
		`{"prompt":"Q1","response":"A1","model":"gpt-4o","ttlSeconds":3600},` +
		`{"prompt":"Q2","response":"A2","model":"gpt-4o","ttlSeconds":3600},` +
		`{"prompt":"Q3","response":"A3","model":"gpt-4o","ttlSeconds":3600}` +
		`],"dryRun":` + dr + `}`
}

// Happy path

// TestPrewarm_HappyPath_3Entries locks the happy path: 3 entries → 3 HSETs →
// response counts match (written=3, skipped=0, errors=0).
func TestPrewarm_HappyPath_3Entries(t *testing.T) {
	// AI GW mock returns 3 written entries.
	gw := mockAIGW(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"written":3,"skipped":0,"errors":0,"embeddingCalls":3,"embeddingCostUsd":0.0001,"durationMs":120,"dryRun":false,"entries":[{"index":0,"written":true},{"index":1,"written":true},{"index":2,"written":true}]}`))
	})

	e := echo.New()
	h := buildPrewarmHandler(t, gw.URL, nil)
	e.POST("/api/admin/semantic-cache/prewarm", h.PrewarmCache)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/semantic-cache/prewarm",
		strings.NewReader(threePrewarmEntries(false)))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if written, _ := result["written"].(float64); int(written) != 3 {
		t.Errorf("written: want 3, got %v", result["written"])
	}
	if skipped, _ := result["skipped"].(float64); int(skipped) != 0 {
		t.Errorf("skipped: want 0, got %v", result["skipped"])
	}
	if errors, _ := result["errors"].(float64); int(errors) != 0 {
		t.Errorf("errors: want 0, got %v", result["errors"])
	}
}

// TestPrewarm_DryRun_3Entries locks the dryRun path: 3 entries embedded but
// no HSET — AI GW returns skipped=3, written=0.
func TestPrewarm_DryRun_3Entries(t *testing.T) {
	gw := mockAIGW(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify dryRun=true was forwarded.
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["dryRun"] != true {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"written":0,"skipped":3,"errors":0,"embeddingCalls":0,"durationMs":5,"dryRun":true,"entries":[{"index":0,"skipped":true,"skipReason":"dry_run"},{"index":1,"skipped":true,"skipReason":"dry_run"},{"index":2,"skipped":true,"skipReason":"dry_run"}]}`))
	})

	e := echo.New()
	h := buildPrewarmHandler(t, gw.URL, nil)
	e.POST("/api/admin/semantic-cache/prewarm", h.PrewarmCache)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/semantic-cache/prewarm",
		strings.NewReader(threePrewarmEntries(true)))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dr, _ := result["dryRun"].(bool); !dr {
		t.Errorf("dryRun: want true, got %v", result["dryRun"])
	}
	if written, _ := result["written"].(float64); int(written) != 0 {
		t.Errorf("written: want 0, got %v", result["written"])
	}
	if skipped, _ := result["skipped"].(float64); int(skipped) != 3 {
		t.Errorf("skipped: want 3, got %v", result["skipped"])
	}
}

// L1 disabled / gateway unavailable

// TestPrewarm_L1Disabled_503 locks the semantic-cache-disabled case: when
// the store reports enabled=false the handler returns 503 without calling
// the AI GW.
func TestPrewarm_L1Disabled_503(t *testing.T) {
	// AI GW should NOT be called — verify by tracking call count.
	gwCalled := 0
	gw := mockAIGW(t, func(w http.ResponseWriter, _ *http.Request) {
		gwCalled++
		w.WriteHeader(http.StatusOK)
	})

	// Store with enabled=false.
	disabledRow := testSemanticCacheConfigRow(false)
	store := &stubSemanticStore{
		current: &disabledRow,
	}

	e := echo.New()
	h := buildPrewarmHandler(t, gw.URL, store)
	e.POST("/api/admin/semantic-cache/prewarm", h.PrewarmCache)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/semantic-cache/prewarm",
		strings.NewReader(singlePrewarmEntry()))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "semantic_cache_disabled") {
		t.Errorf("body missing code: %s", rec.Body.String())
	}
	if gwCalled != 0 {
		t.Errorf("AI GW should not be called when cache is disabled, got %d calls", gwCalled)
	}
}

// TestPrewarm_GatewayUnavailable_503 locks the AI GW 503 path: when the AI
// GW returns 503 (writer nil), the CP proxies the 503 back to the admin.
func TestPrewarm_GatewayUnavailable_503(t *testing.T) {
	gw := mockAIGW(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"semantic cache disabled or Redis unavailable","code":"semantic_cache_disabled"}`))
	})

	e := echo.New()
	h := buildPrewarmHandler(t, gw.URL, nil)
	e.POST("/api/admin/semantic-cache/prewarm", h.PrewarmCache)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/semantic-cache/prewarm",
		strings.NewReader(singlePrewarmEntry()))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestPrewarm_NoAIGatewayURL_503 locks the unconfigured AI GW URL case.
func TestPrewarm_NoAIGatewayURL_503(t *testing.T) {
	e := echo.New()
	h := buildPrewarmHandler(t, "" /* no URL */, nil)
	e.POST("/api/admin/semantic-cache/prewarm", h.PrewarmCache)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/semantic-cache/prewarm",
		strings.NewReader(singlePrewarmEntry()))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gateway_unavailable") {
		t.Errorf("body missing code: %s", rec.Body.String())
	}
}

// TestPrewarm_TooManyEntries_413 locks the 500-entry cap: sending 501 entries
// returns 413 Request Entity Too Large.
func TestPrewarm_TooManyEntries_413(t *testing.T) {
	entries := make([]map[string]any, 501)
	for i := range entries {
		entries[i] = map[string]any{
			"prompt":     "q",
			"response":   "a",
			"model":      "gpt-4o",
			"ttlSeconds": 3600,
		}
	}
	body, _ := json.Marshal(map[string]any{
		"entries": entries,
		"dryRun":  false,
	})

	e := echo.New()
	h := buildPrewarmHandler(t, "http://localhost:9999", nil)
	e.POST("/api/admin/semantic-cache/prewarm", h.PrewarmCache)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/semantic-cache/prewarm",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "entries_too_many") {
		t.Errorf("body missing code: %s", rec.Body.String())
	}
}

// TestPrewarm_EmptyEntries_400 locks the empty-entries guard.
func TestPrewarm_EmptyEntries_400(t *testing.T) {
	e := echo.New()
	h := buildPrewarmHandler(t, "http://localhost:9999", nil)
	e.POST("/api/admin/semantic-cache/prewarm", h.PrewarmCache)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/semantic-cache/prewarm",
		strings.NewReader(`{"entries":[]}`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestPrewarm_EmptyPrompt_400 locks the empty-prompt guard.
func TestPrewarm_EmptyPrompt_400(t *testing.T) {
	e := echo.New()
	h := buildPrewarmHandler(t, "http://localhost:9999", nil)
	e.POST("/api/admin/semantic-cache/prewarm", h.PrewarmCache)

	body := `{"entries":[{"prompt":"","response":"some answer","model":"gpt-4o","ttlSeconds":3600}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/semantic-cache/prewarm",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "prompt") {
		t.Errorf("body missing prompt reference: %s", rec.Body.String())
	}
}

// TestPrewarm_EmptyResponse_400 locks the empty-response guard.
func TestPrewarm_EmptyResponse_400(t *testing.T) {
	e := echo.New()
	h := buildPrewarmHandler(t, "http://localhost:9999", nil)
	e.POST("/api/admin/semantic-cache/prewarm", h.PrewarmCache)

	body := `{"entries":[{"prompt":"What is Go?","response":"","model":"gpt-4o","ttlSeconds":3600}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/semantic-cache/prewarm",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "response") {
		t.Errorf("body missing response reference: %s", rec.Body.String())
	}
}

// TestPrewarm_InvalidTTL_400 locks the TTL range guard: ttlSeconds < 60
// (excluding zero, which is now the "omitted → use default 86400" sentinel)
// and ttlSeconds > 604800 both return 400.
func TestPrewarm_InvalidTTL_400(t *testing.T) {
	tests := []struct {
		name string
		ttl  int
	}{
		{"too_low", 59},
		{"negative", -1},
		{"too_high", 604801},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			h := buildPrewarmHandler(t, "http://localhost:9999", nil)
			e.POST("/api/admin/semantic-cache/prewarm", h.PrewarmCache)

			bodyStr, _ := json.Marshal(map[string]any{
				"entries": []map[string]any{
					{"prompt": "Q", "response": "A", "model": "gpt-4o", "ttlSeconds": tc.ttl},
				},
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/admin/semantic-cache/prewarm",
				strings.NewReader(string(bodyStr)))
			req.Header.Set("Content-Type", "application/json")
			e.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "ttlSeconds") {
				t.Errorf("body missing ttlSeconds reference: %s", rec.Body.String())
			}
		})
	}
}

// TestPrewarm_MalformedJSON_400 locks the bad JSON guard.
func TestPrewarm_MalformedJSON_400(t *testing.T) {
	e := echo.New()
	h := buildPrewarmHandler(t, "http://localhost:9999", nil)
	e.POST("/api/admin/semantic-cache/prewarm", h.PrewarmCache)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/semantic-cache/prewarm",
		strings.NewReader(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestPrewarm_AIGWUnreachable_502 locks the gateway-down path: when the
// AI GW is not listening, the handler returns 502.
func TestPrewarm_AIGWUnreachable_502(t *testing.T) {
	// Closed server — nothing listening on this URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // immediately close so the port is not listening

	e := echo.New()
	h := buildPrewarmHandler(t, srv.URL, nil)
	e.POST("/api/admin/semantic-cache/prewarm", h.PrewarmCache)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/semantic-cache/prewarm",
		strings.NewReader(singlePrewarmEntry()))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gateway_unreachable") {
		t.Errorf("body missing code: %s", rec.Body.String())
	}
}

// TestPrewarm_PartialSuccess locks the partial-success path: AI GW returns
// mixed written/skipped/error entries — CP proxies the response verbatim.
func TestPrewarm_PartialSuccess(t *testing.T) {
	gw := mockAIGW(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"written":1,"skipped":1,"errors":1,"embeddingCalls":2,"durationMs":80,"dryRun":false,"entries":[{"index":0,"written":true},{"index":1,"skipped":true,"skipReason":"embedding_provider_error"},{"index":2,"error":"timeout"}]}`))
	})

	e := echo.New()
	h := buildPrewarmHandler(t, gw.URL, nil)
	e.POST("/api/admin/semantic-cache/prewarm", h.PrewarmCache)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/semantic-cache/prewarm",
		strings.NewReader(threePrewarmEntries(false)))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if written, _ := result["written"].(float64); int(written) != 1 {
		t.Errorf("written: want 1, got %v", result["written"])
	}
	if errs, _ := result["errors"].(float64); int(errs) != 1 {
		t.Errorf("errors: want 1, got %v", result["errors"])
	}
}

// TestPrewarm_RegistersRoute locks the route registration: the prewarm route
// must be among those mounted by RegisterSemanticCacheRoutes.
func TestPrewarm_RegistersRoute(t *testing.T) {
	h := buildPrewarmHandler(t, "http://localhost:9999", nil)
	e := echo.New()
	g := e.Group("/api/admin")
	noop := func(string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterSemanticCacheRoutes(g, noop)

	var found bool
	for _, r := range e.Routes() {
		if r.Method == http.MethodPost && r.Path == "/api/admin/semantic-cache/prewarm" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("POST /api/admin/semantic-cache/prewarm not registered")
	}
}

// testSemanticCacheConfigRow creates a SemanticCacheConfigRow with the given
// enabled state for use in store doubles.
func testSemanticCacheConfigRow(enabled bool) configstore.SemanticCacheConfigRow {
	return configstore.SemanticCacheConfigRow{
		ID:             "singleton",
		RedisIndexName: "nexus:semantic-cache:v1",
		Enabled:        enabled,
	}
}

// TestPrewarm_AttachesBearer verifies the prewarm forwarder carries
// Authorization: Bearer <token> on the CP→ai-gateway /internal/semantic-prewarm
// call.
func TestPrewarm_AttachesBearer(t *testing.T) {
	const tok = "cp-internal-token"
	var gotAuth string
	gw := mockAIGW(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"written":1,"skipped":0,"errors":0,"dryRun":false,"entries":[{"index":0,"written":true}]}`))
	})

	h := cache.NewSemanticCacheHandler(cache.SemanticCacheHandlerDeps{
		AIGatewayURL:           gw.URL,
		AIGatewayInternalToken: tok,
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	e := echo.New()
	e.POST("/api/admin/semantic-cache/prewarm", h.PrewarmCache)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/semantic-cache/prewarm",
		strings.NewReader(singlePrewarmEntry()))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if want := "Bearer " + tok; gotAuth != want {
		t.Errorf("Authorization = %q; want %q", gotAuth, want)
	}
}

// TestPrewarm_ForcesCorpusScope_IgnoresCallerVKScope is the regression:
// a caller that supplies a victim VK's vkScope must NOT have it forwarded — the
// handler forces every entry to the reserved "corpus" scope so planted content
// can never be served under a real VK's cache lane.
func TestPrewarm_ForcesCorpusScope_IgnoresCallerVKScope(t *testing.T) {
	var fwd struct {
		Entries []struct {
			VKScope string `json:"vkScope"`
			Prompt  string `json:"prompt"`
		} `json:"entries"`
	}
	gw := mockAIGW(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&fwd)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"written":1,"skipped":0,"errors":0,"entries":[{"index":0,"written":true}]}`))
	})

	e := echo.New()
	h := buildPrewarmHandler(t, gw.URL, nil)
	e.POST("/api/admin/semantic-cache/prewarm", h.PrewarmCache)

	// Attacker attempts to plant a poisoned response under a victim VK's scope.
	body := `{"entries":[{"prompt":"q","response":"poison","model":"gpt-4o","ttlSeconds":3600,"vkScope":"v1:vk:victim"}],"dryRun":false}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/semantic-cache/prewarm", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(fwd.Entries) != 1 {
		t.Fatalf("expected 1 forwarded entry, got %d", len(fwd.Entries))
	}
	if fwd.Entries[0].VKScope != "corpus" {
		t.Errorf("SEC-C4-01: forwarded vkScope = %q; want forced 'corpus' — caller's victim scope must be dropped", fwd.Entries[0].VKScope)
	}
}
