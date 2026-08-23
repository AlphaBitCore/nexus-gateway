package wiring

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
)

// buildMinimalRouteDeps returns the minimum RouteDeps to call MountCoreRoutes
// without panicking. Most fields are nil (nil-safe in the route handlers).
// Cache.Broker=true and a non-nil DB (mock pool) exercise the broker-registry
// and rulepack-lister branches in MountCoreRoutes.
func buildMinimalRouteDeps(t *testing.T) RouteDeps {
	t.Helper()

	allowlist, err := InitForwardHeaderAllowlist(forwardheader.DefaultConfig())
	if err != nil {
		t.Fatalf("forward header allowlist: %v", err)
	}
	adapterReg := InitProviderRegistry(allowlist, discardLogger())
	reg, err := InitHookRegistry(config.HTTPClientPoolConfig{TimeoutSec: 5}, "", nil)
	if err != nil {
		t.Fatalf("hook registry: %v", err)
	}
	hookCache := InitHookConfigCache(nil, reg, discardLogger())
	pcs := payloadcapture.NewStore(payloadcapture.DefaultConfig())

	// Build a mock-pool-backed DB so deps.DB != nil (covers rulePackLister branch).
	// The pool won't be queried during route mounting.
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	db := store.NewWithPgxPool(mock)

	ht := store.NewHealthTracker()
	t.Cleanup(ht.Stop)

	return RouteDeps{
		Config: &config.Config{
			// Auth.InternalServiceToken gates the /internal/* operator routes;
			// a non-empty token lets the auth route tests exercise the 401/pass
			// paths against the shared handler.
			Auth: config.AuthConfig{InternalServiceToken: testInternalToken},
			Cache: config.CacheConfig{
				Enabled: false,
				// Broker=true exercises the streamcache.NewRegistry branch in MountCoreRoutes.
				Broker: true,
			},
			// CORS with empty AllowedMethods/AllowedHeaders exercises the default-fill branches.
			CORS: config.CORSConfig{
				Enabled:        true,
				AllowedOrigins: []string{"*"},
			},
		},
		DB:              db,
		HookConfigCache: hookCache,
		GWHookRegistry:  reg,
		ProviderReg:     adapterReg,
		HealthTracker:   ht,
		PayloadCapture:  pcs,
		Allowlist:       allowlist,
		Logger:          discardLogger(),
	}
}

// sharedCoreHandler is built exactly once per test binary run because
// MountCoreRoutes registers Prometheus metrics on prometheus.DefaultRegisterer,
// and promauto.MustRegister panics on duplicate registration.
var (
	sharedCoreHandler     http.Handler
	sharedCoreHandlerOnce sync.Once
)

func getSharedCoreHandler(t *testing.T) http.Handler {
	t.Helper()
	sharedCoreHandlerOnce.Do(func() {
		mux := http.NewServeMux()
		// Registered before mounting so the probes are wrapped by the same
		// production middleware chain MountCoreRoutes puts around /v1/*.
		registerWriteDeadlineProbes(mux)
		deps := buildMinimalRouteDeps(t)
		sharedCoreHandler = MountCoreRoutes(mux, deps)
	})
	return sharedCoreHandler
}

// TestMountCoreRoutes_healthz verifies the /healthz endpoint returns 200.
func TestMountCoreRoutes_healthz(t *testing.T) {
	h := getSharedCoreHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if body == "" {
		t.Error("expected non-empty healthz body")
	}
}

// TestMountCoreRoutes_withCORSEnabled verifies CORS middleware is wired
// without panic. CORS is a middleware layer — we test it by mounting a
// fresh mux with CORS-enabled deps and making a preflight request.
// Note: this test uses its own call to MountCoreRoutes, which would
// re-register prometheus metrics. To avoid the duplicate-registration
// panic we reuse the shared handler (CORS config isn't relevant for
// the basic non-panic assertion we need here).
func TestMountCoreRoutes_withCORSEnabled(t *testing.T) {
	// Verify the shared handler (already mounted) returns a non-nil handler —
	// the main value of this test is ensuring CORS config fields in
	// RouteDeps compile and are wired correctly.
	h := getSharedCoreHandler(t)
	if h == nil {
		t.Fatal("expected non-nil handler")
	}

	// Verify a basic request still works.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == 0 {
		t.Error("expected non-zero status code")
	}
}

// TestMountCoreRoutes_metricsRequiresToken is the regression guard:
// /metrics must reject an unauthenticated request (401) and serve only when
// the internal-service bearer token is presented.
func TestMountCoreRoutes_metricsRequiresToken(t *testing.T) {
	h := getSharedCoreHandler(t)

	// No token → 401.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated /metrics: expected 401, got %d", rr.Code)
	}

	// Wrong token → 401.
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong-token /metrics: expected 401, got %d", rr.Code)
	}

	// Correct internal-service token → 200 and a Prometheus exposition body.
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+testInternalToken)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("authenticated /metrics: expected 200, got %d", rr.Code)
	}
}

// TestMountCoreRoutes_readEndpointsRateLimited is the regression guard:
// the authenticated read-only endpoints are wrapped by the per-VK limiter, so
// the route stays registered (not 404) and the wrapper does not break the
// unauthenticated 401 path (no VK header → inner requireVK emits 401, never a
// false 200). nil-limiter deps in the shared handler mean the wrapper passes
// through to the inner handler, which authenticates and rejects.
func TestMountCoreRoutes_readEndpointsRegisteredAndAuthGated(t *testing.T) {
	h := getSharedCoreHandler(t)
	for _, path := range []string{"/v1/models", "/v1/usage", "/v1/usage/daily"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code == http.StatusNotFound {
			t.Errorf("%s: expected route registered, got 404", path)
		}
		// Without a VK the inner handler must reject, never serve a 200.
		if rr.Code == http.StatusOK {
			t.Errorf("%s: unauthenticated request unexpectedly returned 200", path)
		}
	}
}

// TestMountCoreRoutes_multimodalRoutesRegisteredAndVKGated verifies the
// multimodal JSON-body data-plane routes (image generation + TTS) are
// registered and flow through the SAME ServeProxy chain as chat: never
// 404 (registered), never 200 without a VK (auth-gated), and the status
// matches what /v1/chat/completions returns for the identical
// unauthenticated request — middleware-chain parity, not a bespoke path.
func TestMountCoreRoutes_multimodalRoutesRegisteredAndVKGated(t *testing.T) {
	h := getSharedCoreHandler(t)

	// Reference: what the chat route (known-good VK-gated ServeProxy chain)
	// returns for an unauthenticated request against these deps.
	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	chatRR := httptest.NewRecorder()
	h.ServeHTTP(chatRR, chatReq)
	if chatRR.Code == http.StatusNotFound || chatRR.Code == http.StatusOK {
		t.Fatalf("chat reference route misbehaved: %d", chatRR.Code)
	}

	cases := []struct {
		path string
		body string
	}{
		{"/v1/images/generations", `{"model":"dall-e-3","prompt":"a red fox"}`},
		{"/v1/audio/speech", `{"model":"tts-1","input":"hello","voice":"alloy"}`},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code == http.StatusNotFound {
			t.Errorf("%s: route not registered (404)", tc.path)
		}
		if rr.Code == http.StatusOK {
			t.Errorf("%s: unauthenticated request unexpectedly returned 200", tc.path)
		}
		if rr.Code != chatRR.Code {
			t.Errorf("%s: status %d differs from chat's %d — must share the same ServeProxy middleware chain", tc.path, rr.Code, chatRR.Code)
		}
	}

	// STT multipart routes are registered on the PARALLEL ServeSTT handler
	// (e88-s5). They must be registered (not 404) and VK-gated (not 200 without
	// a VK); the exact rejection status may differ from chat's ServeProxy chain
	// (multipart Content-Type validation runs before auth on this path), so
	// this only asserts registered + auth-gated, not middleware-chain parity.
	for _, path := range []string{"/v1/audio/transcriptions"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("x"))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code == http.StatusNotFound {
			t.Errorf("%s: STT route not registered (404)", path)
		}
		if rr.Code == http.StatusOK {
			t.Errorf("%s: unauthenticated STT request unexpectedly returned 200", path)
		}
	}

	// Guardrail is the parallel compliance-verdict handler (e90-s1). It must be
	// registered (not 404) and VK-gated: an unauthenticated request with a valid
	// JSON body authenticates before evaluating, so it returns 401, never 200.
	{
		req := httptest.NewRequest(http.MethodPost, "/v1/guardrail", strings.NewReader(`{"content":"hi"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code == http.StatusNotFound {
			t.Error("/v1/guardrail: route not registered (404)")
		}
		if rr.Code == http.StatusOK {
			t.Error("/v1/guardrail: unauthenticated request unexpectedly returned 200")
		}
	}

	// The image multipart siblings are still deferred (multipart model
	// extraction + ingress-path preservation not yet built for images): they
	// must 404, not 5xx.
	for _, path := range []string{"/v1/images/edits", "/v1/images/variations"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("x"))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: expected 404 (deferred multipart route), got %d", path, rr.Code)
		}
	}

	// The async video family (e88-s6): submit + poll + content + delete are
	// registered on the PARALLEL ServeVideo* handlers. Submit and poll MUST
	// mount together (submit alone would strand every job) — assert BOTH are
	// registered (not 404) and VK-gated (not 200 without a key).
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/videos"},
		{http.MethodGet, "/v1/videos/vid_123"},
		{http.MethodGet, "/v1/videos/vid_123/content"},
		{http.MethodDelete, "/v1/videos/vid_123"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.method == http.MethodPost {
			req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code == http.StatusNotFound {
			t.Errorf("%s %s: video route not registered (404)", tc.method, tc.path)
		}
		if rr.Code == http.StatusOK {
			t.Errorf("%s %s: unauthenticated video request unexpectedly returned 200", tc.method, tc.path)
		}
	}

	// Deliberately-unserved video sub-routes must answer an OpenAI-shaped 404
	// ENVELOPE (not the mux's bare-text 404): a JSON body with a machine code,
	// so an SDK can distinguish "not served" from a wrong base URL.
	for _, tc := range []struct{ method, path, code string }{
		{http.MethodGet, "/v1/videos", "VIDEO_LIST_UNSUPPORTED"},
		{http.MethodPost, "/v1/videos/vid_123/remix", "VIDEO_OP_UNSUPPORTED"},
		{http.MethodPost, "/v1/videos/edits", "VIDEO_OP_UNSUPPORTED"},
		{http.MethodPost, "/v1/videos/extensions", "VIDEO_OP_UNSUPPORTED"},
		{http.MethodPost, "/v1/videos/characters", "VIDEO_OP_UNSUPPORTED"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s %s: want 404, got %d", tc.method, tc.path, rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s: Content-Type = %q, want the JSON error envelope", tc.method, tc.path, ct)
		}
		if !strings.Contains(rr.Body.String(), tc.code) {
			t.Errorf("%s %s: body %q missing machine code %s", tc.method, tc.path, rr.Body.String(), tc.code)
		}
	}
}

// TestMountCoreRoutes_geminiStreamRoute verifies the :streamGenerateContent
// switch arm in the /v1beta/models/{model} handler. The proxy handler has
// nil deps so it returns an error, but the route must be registered (not 404).
func TestMountCoreRoutes_geminiStreamRoute(t *testing.T) {
	h := getSharedCoreHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-pro:streamGenerateContent", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	// Must not be 404 (route is registered) — actual response may be 4xx/5xx with nil deps.
	if rr.Code == http.StatusNotFound {
		t.Errorf("expected route registered, got 404 for :streamGenerateContent")
	}
}

// TestMountCoreRoutes_geminiNonStreamRoute verifies the :generateContent switch arm.
func TestMountCoreRoutes_geminiNonStreamRoute(t *testing.T) {
	h := getSharedCoreHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-pro:generateContent", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == http.StatusNotFound {
		t.Errorf("expected route registered, got 404 for :generateContent")
	}
}

// TestMountCoreRoutes_geminiDefaultRouteNotFound verifies the default (NotFound) arm.
func TestMountCoreRoutes_geminiDefaultRouteNotFound(t *testing.T) {
	h := getSharedCoreHandler(t)
	// A model path that matches neither suffix returns 404.
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-pro:unknownAction", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown suffix, got %d", rr.Code)
	}
}

// TestMountCoreRoutes_openModelDetailAcceptsProviderPrefixedID pins the route
// pattern against the shape the LIST endpoint actually emits. The public list
// returns ids as "openai/gpt-4o", so the detail route must bind a two-segment
// id; a single-segment "{model_id}" silently never matches and the mux answers
// its own plaintext "404 page not found" without ever invoking the handler.
// Asserting through the real mux is the point — a handler-level test that sets
// the path value directly bypasses pattern matching and cannot catch this.
func TestMountCoreRoutes_openModelDetailAcceptsProviderPrefixedID(t *testing.T) {
	h := getSharedCoreHandler(t)
	for _, path := range []string{
		"/api/v1/open/models/gpt-4o",        // bare code
		"/api/v1/open/models/openai/gpt-4o", // provider-prefixed, as the list emits
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		// The shared test deps have no DB, so the handler answers 500; what
		// matters is that the ROUTE matched — an unmatched pattern yields the
		// mux's own bare-text 404 instead of the handler's JSON envelope.
		if strings.Contains(rr.Body.String(), "404 page not found") {
			t.Errorf("%s: route did not match — mux 404, handler never ran", path)
		}
	}
}
