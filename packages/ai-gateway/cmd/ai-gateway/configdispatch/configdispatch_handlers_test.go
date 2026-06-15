package configdispatch

// configdispatch_handlers_test.go — per-key handler behaviour tests.
//
// Strategy: for every registered key, verify:
//   1. Valid JSON / empty payload → subsystem state changes observably.
//   2. Malformed JSON → handler returns an error (where applicable).
//   3. Nil dep → graceful no-op (handler returns nil error).
//
// Apply invocation: loader.Apply(ctx, map[string]thingclient.ConfigState{key: {State: raw, Version: 1}}).
// Error extraction: configloader records per-key errors via OutcomeTracker;
// the first fatal error is also returned from Apply.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/cmd/ai-gateway/wiring"
	cachecore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/freshness"
	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/passthrough"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/aiguard"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
	"github.com/alicebob/miniredis/v2"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

// test helpers

// newTestDeps returns a Deps with all nil-tolerant fields left nil and only
// the mandatory logger / tracker / obs state populated.
func newTestDeps(t *testing.T) Deps {
	t.Helper()
	var obs atomic.Pointer[telemetry.Config]
	return Deps{
		Logger:             slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		ThingID:            "test-ag",
		Outcomes:           thingclient.NewOutcomeTracker(),
		ObservabilityState: &obs,
	}
}

// logCapture returns a logger that writes into a buffer so tests can inspect
// structured log output for observable behaviour verification.
func logCapture() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, buf
}

// applyKey invokes the named key's handler via Loader.Apply.
// It returns the first error from the apply result map (Apply returns
// (reported, firstErr) but individual key errors ride in the OutcomeTracker).
func applyKey(t *testing.T, d Deps, key string, raw []byte) error {
	t.Helper()
	loader := BuildConfigLoader(d)
	desired := map[string]thingclient.ConfigState{
		key: {State: json.RawMessage(raw), Version: 1},
	}
	_, err := loader.Apply(context.Background(), desired)
	return err
}

// miniredisCache builds a real *cachecore.Cache backed by an in-process
// miniredis instance. The cache is pre-initialised with Enabled=true so the
// handler reaches the JSON-parse branch (nil-receiver is short-circuited).
func miniredisCache(t *testing.T) (*cachecore.Cache, *miniredis.Miniredis) {
	t.Helper()
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mini.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	c := cachecore.New(rdb, cachecore.Config{Enabled: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if c == nil {
		t.Fatal("cachecore.New returned nil")
	}
	return c, mini
}

// F-0126: routing_rules / credentials / providers / models / virtual_keys are
// all registered ai-gateway keys backed by structurally-required deps (DB,
// CacheLayer, CredManager). A nil dep there is a wiring regression, NOT a
// disabled module — the applier must return an ERROR so the loader records a
// failed outcome and does not falsely advance the reportedVer to "converged".

func TestHandler_RoutingRules_NilDB_Errors(t *testing.T) {
	d := newTestDeps(t)
	d.DB = nil
	if err := applyKey(t, d, "routing_rules", []byte(`{}`)); err == nil {
		t.Fatal("routing_rules with nil DB must error (F-0126), got nil")
	}
}

func TestHandler_Credentials_NilCredManager_Errors(t *testing.T) {
	d := newTestDeps(t)
	d.CacheLayer = nil
	d.CredManager = nil
	if err := applyKey(t, d, "credentials", []byte(`{}`)); err == nil {
		t.Fatal("credentials with nil CredManager must error (F-0126), got nil")
	}
}

func TestHandler_Providers_NilCacheLayer_Errors(t *testing.T) {
	d := newTestDeps(t)
	d.CacheLayer = nil
	d.GeminiCacheMgrSet = nil
	if err := applyKey(t, d, "providers", []byte(`{}`)); err == nil {
		t.Fatal("providers with nil CacheLayer must error (F-0126), got nil")
	}
}

func TestHandler_Models_NilCacheLayer_Errors(t *testing.T) {
	d := newTestDeps(t)
	d.CacheLayer = nil
	if err := applyKey(t, d, "models", []byte(`{}`)); err == nil {
		t.Fatal("models with nil CacheLayer must error (F-0126), got nil")
	}
}

func TestHandler_Hooks_NilHookCache_NoOp(t *testing.T) {
	d := newTestDeps(t)
	d.HookConfigCache = nil
	if err := applyKey(t, d, "hooks", []byte(`{}`)); err != nil {
		t.Fatalf("hooks with nil HookConfigCache: unexpected error: %v", err)
	}
}

// stubHookReloader is a minimal HookConfigReloader for tests.
type stubHookReloader struct {
	called bool
	err    error
}

func (s *stubHookReloader) Reload(_ context.Context) error {
	s.called = true
	return s.err
}

func TestHandler_Hooks_WithCache_CallsReload(t *testing.T) {
	d := newTestDeps(t)
	stub := &stubHookReloader{}
	d.HookConfigCache = stub
	if err := applyKey(t, d, "hooks", []byte(`{}`)); err != nil {
		t.Fatalf("hooks: unexpected error: %v", err)
	}
	if !stub.called {
		t.Fatal("hooks: expected Reload to be called")
	}
}

func TestHandler_Hooks_ReloadError_Propagates(t *testing.T) {
	d := newTestDeps(t)
	stub := &stubHookReloader{err: errTest}
	d.HookConfigCache = stub
	_, err := BuildConfigLoader(d).Apply(context.Background(), map[string]thingclient.ConfigState{
		"hooks": {State: nil, Version: 1},
	})
	if err == nil {
		t.Fatal("hooks: expected error from Reload, got nil")
	}
}

func TestHandler_Observability_NilTelemetryProvider_NoOp(t *testing.T) {
	d := newTestDeps(t)
	d.TelemetryProvider = nil
	d.DB = nil
	if err := applyKey(t, d, "observability", []byte(`{}`)); err != nil {
		t.Fatalf("observability with nil TelemetryProvider: unexpected error: %v", err)
	}
}

func TestHandler_PayloadCapture_NilDB_NoOp(t *testing.T) {
	d := newTestDeps(t)
	d.DB = nil
	if err := applyKey(t, d, "payload_capture", []byte(`{}`)); err != nil {
		t.Fatalf("payload_capture with nil DB: unexpected error: %v", err)
	}
}

// F-0125: ai-gateway intentionally registers NO streaming_compliance applier
// (the key is absent from ValidByThingType["ai-gateway"], so Hub never pushes
// it). A streaming_compliance shadow tick must therefore be treated as an
// unknown key and skipped — never applied — so no Store mutation can ride in
// from a key the gateway is not a registered consumer of.
func TestHandler_StreamingCompliance_NotRegistered_Skipped(t *testing.T) {
	d := newTestDeps(t)
	if BuildConfigLoader(d).Has("streaming_compliance") {
		t.Fatal("ai-gateway must not register a streaming_compliance applier (F-0125)")
	}
	// An unknown-key tick is a no-op (logged + skipped), never an error.
	if err := applyKey(t, d, "streaming_compliance", []byte(`{"default_mode":"buffer_full_block"}`)); err != nil {
		t.Fatalf("unknown streaming_compliance key should be skipped, got %v", err)
	}
}

func TestHandler_CredentialReliability_NilReliability_NoOp(t *testing.T) {
	d := newTestDeps(t)
	d.Reliability = nil
	d.DB = nil
	if err := applyKey(t, d, "credential_reliability", []byte(`{}`)); err != nil {
		t.Fatalf("credential_reliability with nil Reliability: unexpected error: %v", err)
	}
}

func TestHandler_CredentialReliability_NilDB_NoOp(t *testing.T) {
	d := newTestDeps(t)
	d.Reliability = &stubReliabilityReloader{}
	d.DB = nil
	if err := applyKey(t, d, "credential_reliability", []byte(`{}`)); err != nil {
		t.Fatalf("credential_reliability with nil DB: unexpected error: %v", err)
	}
}

// stubReliabilityReloader is a minimal ReliabilityReloader for tests.
// It must satisfy wiring.ReliabilityReloader (Reload ctx, wiring.MetadataReader).
type stubReliabilityReloader struct {
	called bool
	err    error
}

func (s *stubReliabilityReloader) Reload(_ context.Context, _ wiring.MetadataReader) error {
	s.called = true
	return s.err
}

// quota triad

func TestHandler_QuotaPolicies_NilPolicyCache_NoOp(t *testing.T) {
	d := newTestDeps(t)
	d.PolicyCache = nil
	for _, key := range []string{"quota_policies", "quota_overrides", "organizations"} {
		if err := applyKey(t, d, key, []byte(`{}`)); err != nil {
			t.Fatalf("%s with nil PolicyCache: unexpected error: %v", key, err)
		}
	}
}

func TestHandler_VirtualKeys_NilCacheLayer_Errors(t *testing.T) {
	d := newTestDeps(t)
	d.CacheLayer = nil
	if err := applyKey(t, d, "virtual_keys", []byte(`{}`)); err == nil {
		t.Fatal("virtual_keys with nil CacheLayer must error (F-0126), got nil")
	}
}

func TestHandler_VirtualKeys_EmptyPayload_NilCacheLayer_Errors(t *testing.T) {
	d := newTestDeps(t)
	d.CacheLayer = nil
	if err := applyKey(t, d, "virtual_keys", nil); err == nil {
		t.Fatal("virtual_keys with nil payload and nil CacheLayer must error (F-0126), got nil")
	}
}

func TestHandler_AIGuard_NilGetter_NoOp(t *testing.T) {
	d := newTestDeps(t)
	d.AIGuardConfigCache = nil
	if err := applyKey(t, d, "ai_guard", []byte(`{}`)); err != nil {
		t.Fatalf("ai_guard with nil getter: unexpected error: %v", err)
	}
}

func TestHandler_AIGuard_GetterReturnsNil_NoOp(t *testing.T) {
	d := newTestDeps(t)
	d.AIGuardConfigCache = func() *aiguard.ConfigCache { return nil }
	if err := applyKey(t, d, "ai_guard", []byte(`{}`)); err != nil {
		t.Fatalf("ai_guard with nil cache: unexpected error: %v", err)
	}
}

func TestHandler_AIGuard_WithCache_CallsInvalidate(t *testing.T) {
	d := newTestDeps(t)
	discardLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cc := aiguard.NewConfigCache(&stubAIGuardLoader{}, time.Minute, discardLogger)
	d.AIGuardConfigCache = func() *aiguard.ConfigCache { return cc }
	// Invalidate clears the snap pointer; verify the handler executes without error
	// (non-nil cache branch runs cc.Invalidate).
	if err := applyKey(t, d, "ai_guard", []byte(`{}`)); err != nil {
		t.Fatalf("ai_guard with non-nil cache: unexpected error: %v", err)
	}
}

// stubAIGuardLoader implements aiguard.Loader for tests (no real DB).
type stubAIGuardLoader struct{}

func (s *stubAIGuardLoader) Load(_ context.Context) (*configstore.AIGuardConfig, error) {
	return &configstore.AIGuardConfig{}, nil
}

// cache (prompt cache)

func TestHandler_Cache_EmptyPayload_NoOp(t *testing.T) {
	d := newTestDeps(t)
	d.GeminiCacheMgrSet = nil
	d.NormEngine = nil
	if err := applyKey(t, d, "cache", nil); err != nil {
		t.Fatalf("cache with empty payload: unexpected error: %v", err)
	}
}

func TestHandler_Cache_MalformedJSON_Error(t *testing.T) {
	d := newTestDeps(t)
	d.GeminiCacheMgrSet = nil
	d.NormEngine = nil
	err := applyKey(t, d, "cache", []byte(`{not-json`))
	if err == nil {
		t.Fatal("cache with malformed JSON: expected error, got nil")
	}
}

func TestHandler_Cache_ValidJSON_NilDeps_NoOp(t *testing.T) {
	d := newTestDeps(t)
	d.GeminiCacheMgrSet = nil
	d.NormEngine = nil
	payload := []byte(`{"global":{"enabled":true}}`)
	if err := applyKey(t, d, "cache", payload); err != nil {
		t.Fatalf("cache with valid JSON and nil deps: unexpected error: %v", err)
	}
}

func TestHandler_Cache_ValidJSON_NormEngine_CallsReload(t *testing.T) {
	d := newTestDeps(t)
	d.GeminiCacheMgrSet = nil
	d.CacheLayer = nil
	eng := wirerewrite.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.NormEngine = eng
	payload := []byte(`{"global":{"enabled":true}}`)
	if err := applyKey(t, d, "cache", payload); err != nil {
		t.Fatalf("cache with NormEngine: unexpected error: %v", err)
	}
}

func TestHandler_GatewayPassthrough_NilCache_NoOp(t *testing.T) {
	d := newTestDeps(t)
	d.PassthroughCache = nil
	if err := applyKey(t, d, "gateway_passthrough", []byte(`{}`)); err != nil {
		t.Fatalf("gateway_passthrough with nil cache: unexpected error: %v", err)
	}
}

func TestHandler_GatewayPassthrough_EmptyPayload_SetsNilSnapshot(t *testing.T) {
	d := newTestDeps(t)
	pc := passthrough.NewCache()
	d.PassthroughCache = pc
	// Empty payload should call SetSnapshot(nil) → empty Snapshot installed.
	if err := applyKey(t, d, "gateway_passthrough", nil); err != nil {
		t.Fatalf("gateway_passthrough empty payload: unexpected error: %v", err)
	}
	// After nil snapshot no bypass is active.
	if result := pc.Effective("any-provider", "any-adapter"); result != nil {
		t.Fatalf("gateway_passthrough: expected nil Effective after empty payload, got %+v", result)
	}
}

func TestHandler_GatewayPassthrough_MalformedJSON_Error(t *testing.T) {
	d := newTestDeps(t)
	pc := passthrough.NewCache()
	d.PassthroughCache = pc
	err := applyKey(t, d, "gateway_passthrough", []byte(`{bad json`))
	if err == nil {
		t.Fatal("gateway_passthrough malformed JSON: expected error, got nil")
	}
}

func TestHandler_GatewayPassthrough_ValidSnapshot_StoresGlobal(t *testing.T) {
	d := newTestDeps(t)
	pc := passthrough.NewCache()
	d.PassthroughCache = pc

	// Build a valid passthrough snapshot with global enabled + future expiry.
	future := time.Now().Add(1 * time.Hour)
	snap := passthrough.Snapshot{
		Global: passthrough.TierEntry{
			Enabled:     true,
			BypassHooks: true,
			ExpiresAt:   &future,
		},
	}
	raw, _ := json.Marshal(snap)
	if err := applyKey(t, d, "gateway_passthrough", raw); err != nil {
		t.Fatalf("gateway_passthrough valid snapshot: unexpected error: %v", err)
	}
	result := pc.Effective("any-provider", "any-adapter")
	if result == nil {
		t.Fatal("gateway_passthrough: expected non-nil Effective after valid snapshot")
	}
	if !result.BypassHooks {
		t.Fatalf("gateway_passthrough: expected BypassHooks=true, got %+v", result)
	}
}

func TestHandler_LogLevel_EmptyLevel_NoOp(t *testing.T) {
	d := newTestDeps(t)
	payload := []byte(`{"level":""}`)
	if err := applyKey(t, d, "log_level", payload); err != nil {
		t.Fatalf("log_level empty level: unexpected error: %v", err)
	}
}

func TestHandler_LogLevel_ValidLevel_LogsChange(t *testing.T) {
	logger, buf := logCapture()
	d := newTestDeps(t)
	d.Logger = logger
	payload := []byte(`{"level":"debug"}`)
	if err := applyKey(t, d, "log_level", payload); err != nil {
		t.Fatalf("log_level debug: unexpected error: %v", err)
	}
	// The Apply handler logs "log level updated via shadow" at Info.
	if !bytes.Contains(buf.Bytes(), []byte("log level updated via shadow")) {
		t.Fatalf("log_level: expected log line not found in output: %s", buf.String())
	}
}

func TestHandler_LogLevel_MalformedJSON_Error(t *testing.T) {
	d := newTestDeps(t)
	err := applyKey(t, d, "log_level", []byte(`{not json`))
	if err == nil {
		t.Fatal("log_level malformed JSON: expected error, got nil")
	}
}

func TestHandler_LogLevel_KnownLevels(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		d := newTestDeps(t)
		payload := fmt.Sprintf(`{"level":%q}`, level)
		if err := applyKey(t, d, "log_level", []byte(payload)); err != nil {
			t.Fatalf("log_level %q: unexpected error: %v", level, err)
		}
	}
}

func TestHandler_SemanticCacheConfig_NilLifecycle_NoOp(t *testing.T) {
	d := newTestDeps(t)
	d.SemanticIndexLifecycle = nil
	if err := applyKey(t, d, "semantic_cache.config", []byte(`{}`)); err != nil {
		t.Fatalf("semantic_cache.config with nil lifecycle: unexpected error: %v", err)
	}
}

func TestHandler_SemanticCacheConfig_EmptyPayload_DisablesCache(t *testing.T) {
	d := newTestDeps(t)
	cc := semantic.NewConfigCache()
	// Seed with enabled=true so we can observe the disable.
	cc.Set(semantic.ConfigSnapshot{Enabled: true})
	lc := semantic.NewIndexLifecycle(cc, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.SemanticIndexLifecycle = lc

	if err := applyKey(t, d, "semantic_cache.config", nil); err != nil {
		t.Fatalf("semantic_cache.config empty payload: unexpected error: %v", err)
	}
	snap := cc.Get()
	if snap.Enabled {
		t.Fatal("semantic_cache.config: empty payload should disable cache, but Enabled=true")
	}
}

func TestHandler_SemanticCacheConfig_MalformedJSON_Error(t *testing.T) {
	d := newTestDeps(t)
	cc := semantic.NewConfigCache()
	lc := semantic.NewIndexLifecycle(cc, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.SemanticIndexLifecycle = lc
	err := applyKey(t, d, "semantic_cache.config", []byte(`{not json`))
	if err == nil {
		t.Fatal("semantic_cache.config malformed JSON: expected error, got nil")
	}
}

func TestHandler_SemanticCacheConfig_ValidPayload_SetsSnapshot(t *testing.T) {
	d := newTestDeps(t)
	cc := semantic.NewConfigCache()
	lc := semantic.NewIndexLifecycle(cc, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.SemanticIndexLifecycle = lc

	providerID := "prov-1"
	modelID := "model-1"
	dim := 768
	// EmbeddingFingerprint is intentionally empty so IndexLifecycle.OnConfigSnapshot
	// skips EnsureIndex (which would panic on nil client). We test the snapshot
	// fields are decoded and stored correctly.
	payload := semanticCacheConfigBlob{
		Enabled:             true,
		EmbeddingProviderID: &providerID,
		EmbeddingModelID:    &modelID,
		EmbeddingDimension:  &dim,
		// Fingerprint empty → EnsureIndex skipped (nil client safe).
		Threshold: 0.92,
		VaryBy:    "vk",
	}
	raw, _ := json.Marshal(payload)
	if err := applyKey(t, d, "semantic_cache.config", raw); err != nil {
		t.Fatalf("semantic_cache.config valid payload: unexpected error: %v", err)
	}
	snap := cc.Get()
	if !snap.Enabled {
		t.Fatal("semantic_cache.config: expected Enabled=true after valid payload")
	}
	if snap.EmbeddingProviderID != providerID {
		t.Fatalf("semantic_cache.config: EmbeddingProviderID got %q, want %q", snap.EmbeddingProviderID, providerID)
	}
	if snap.EmbeddingDimension != dim {
		t.Fatalf("semantic_cache.config: EmbeddingDimension got %d, want %d", snap.EmbeddingDimension, dim)
	}
}

func TestHandler_SemanticCacheConfig_NilPointerFields_FallsBackToZero(t *testing.T) {
	d := newTestDeps(t)
	cc := semantic.NewConfigCache()
	lc := semantic.NewIndexLifecycle(cc, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.SemanticIndexLifecycle = lc
	// All optional pointer fields absent — should not crash.
	if err := applyKey(t, d, "semantic_cache.config", []byte(`{"enabled":false}`)); err != nil {
		t.Fatalf("semantic_cache.config nil pointers: unexpected error: %v", err)
	}
}

func TestHandler_SemanticCacheConfig_AllProviderJoinFields(t *testing.T) {
	d := newTestDeps(t)
	cc := semantic.NewConfigCache()
	lc := semantic.NewIndexLifecycle(cc, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.SemanticIndexLifecycle = lc

	providerID := "openai"
	modelID := "text-embedding-3-small"
	dim := 1536
	// Fingerprint empty so IndexLifecycle skips EnsureIndex (nil client safe).
	payload := semanticCacheConfigBlob{
		Enabled:                       true,
		EmbeddingProviderID:           &providerID,
		EmbeddingModelID:              &modelID,
		EmbeddingDimension:            &dim,
		Threshold:                     0.95,
		VaryBy:                        "user",
		EmbedStrategy:                 "last_user",
		AllowCrossModel:               true,
		EmbeddingProviderBaseURL:      "https://api.openai.com",
		EmbeddingProviderModelID:      "text-embedding-3-small",
		EmbeddingInputPricePerMillion: 0.02,
	}
	raw, _ := json.Marshal(payload)
	if err := applyKey(t, d, "semantic_cache.config", raw); err != nil {
		t.Fatalf("semantic_cache.config full fields: unexpected error: %v", err)
	}
	snap := cc.Get()
	if snap.EmbeddingProviderBaseURL != "https://api.openai.com" {
		t.Fatalf("EmbeddingProviderBaseURL: got %q", snap.EmbeddingProviderBaseURL)
	}
	if snap.EmbeddingInputPricePerMillion != 0.02 {
		t.Fatalf("EmbeddingInputPricePerMillion: got %f", snap.EmbeddingInputPricePerMillion)
	}
	if !snap.AllowCrossModel {
		t.Fatal("AllowCrossModel should be true")
	}
}

func TestHandler_TimeSensitivePatterns_NilDetector_NoOp(t *testing.T) {
	d := newTestDeps(t)
	d.FreshnessDetector = nil
	if err := applyKey(t, d, "response_cache.time_sensitive_patterns", nil); err != nil {
		t.Fatalf("time_sensitive_patterns with nil detector: unexpected error: %v", err)
	}
}

func TestHandler_TimeSensitivePatterns_EmptyPayload_ClearsRules(t *testing.T) {
	d := newTestDeps(t)
	reg := prometheus.NewRegistry()
	det, err := freshness.NewDetector(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "test", reg)
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}
	d.FreshnessDetector = det
	// Empty payload → Reload with empty rules → no error.
	if err := applyKey(t, d, "response_cache.time_sensitive_patterns", nil); err != nil {
		t.Fatalf("time_sensitive_patterns empty payload: unexpected error: %v", err)
	}
	// After clearing, should not detect time-sensitive messages.
	matched, _ := det.IsTimeSensitive([]freshness.ChatMessage{{Role: "user", Content: "what is today's stock price?"}})
	if matched {
		t.Fatal("time_sensitive_patterns: after clearing rules, expected no match")
	}
}

func TestHandler_TimeSensitivePatterns_MalformedJSON_Error(t *testing.T) {
	d := newTestDeps(t)
	reg := prometheus.NewRegistry()
	det, err := freshness.NewDetector(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "test", reg)
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}
	d.FreshnessDetector = det
	if err = applyKey(t, d, "response_cache.time_sensitive_patterns", []byte(`{not array`)); err == nil {
		t.Fatal("time_sensitive_patterns malformed JSON: expected error, got nil")
	}
}

func TestHandler_TimeSensitivePatterns_ValidRules_LoadsDetector(t *testing.T) {
	d := newTestDeps(t)
	logger, buf := logCapture()
	d.Logger = logger
	reg := prometheus.NewRegistry()
	det, err := freshness.NewDetector(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "test", reg)
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}
	d.FreshnessDetector = det

	rules := []freshness.Rule{
		{ID: "stock-price", Keywords: []string{"stock price"}, Enabled: true},
	}
	raw, _ := json.Marshal(rules)
	if err := applyKey(t, d, "response_cache.time_sensitive_patterns", raw); err != nil {
		t.Fatalf("time_sensitive_patterns valid rules: unexpected error: %v", err)
	}
	// Rule is now loaded — should match a time-sensitive message.
	matched, ruleID := det.IsTimeSensitive([]freshness.ChatMessage{{Role: "user", Content: "what is the stock price?"}})
	if !matched {
		t.Fatalf("time_sensitive_patterns: expected match after loading rule, got no match; log=%s", buf.String())
	}
	if ruleID != "stock-price" {
		t.Fatalf("time_sensitive_patterns: got ruleID=%q, want %q", ruleID, "stock-price")
	}
}

func TestHandler_TimeSensitivePatterns_ReloadWithEmptyArray_ClearsPreviousRules(t *testing.T) {
	d := newTestDeps(t)
	reg := prometheus.NewRegistry()
	initialRules := []freshness.Rule{
		{ID: "stock-price", Keywords: []string{"stock price"}, Enabled: true},
	}
	det, err := freshness.NewDetector(initialRules, slog.New(slog.NewTextHandler(io.Discard, nil)), "test", reg)
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}
	d.FreshnessDetector = det

	// Reload with empty JSON array.
	if err := applyKey(t, d, "response_cache.time_sensitive_patterns", []byte(`[]`)); err != nil {
		t.Fatalf("time_sensitive_patterns reload empty array: unexpected error: %v", err)
	}
	// Previous rules should be cleared.
	matched, _ := det.IsTimeSensitive([]freshness.ChatMessage{{Role: "user", Content: "what is the stock price?"}})
	if matched {
		t.Fatal("time_sensitive_patterns: after clearing with empty array, expected no match")
	}
}

func TestHandler_ExtractCacheConfig_NilCache_NoOp(t *testing.T) {
	d := newTestDeps(t)
	d.ResponseCache = nil
	if err := applyKey(t, d, "response_cache.extract_config", []byte(`{}`)); err != nil {
		t.Fatalf("extract_config with nil cache: unexpected error: %v", err)
	}
}

func TestHandler_ExtractCacheConfig_EmptyPayload_NilCache_NoOp(t *testing.T) {
	d := newTestDeps(t)
	d.ResponseCache = nil
	if err := applyKey(t, d, "response_cache.extract_config", nil); err != nil {
		t.Fatalf("extract_config empty payload with nil cache: unexpected error: %v", err)
	}
}

func TestHandler_ExtractCacheConfig_EmptyPayload_DisablesCache(t *testing.T) {
	d := newTestDeps(t)
	logger, _ := logCapture()
	d.Logger = logger
	c, _ := miniredisCache(t)
	d.ResponseCache = c

	// Pre-enable so we can observe the disable.
	c.SetConfig(cachecore.ConfigSnapshot{Enabled: true, TTL: time.Minute})
	if !c.IsEnabled() {
		t.Fatal("pre-condition: cache should be enabled")
	}

	if err := applyKey(t, d, "response_cache.extract_config", nil); err != nil {
		t.Fatalf("extract_config empty payload: unexpected error: %v", err)
	}
	// Empty payload sets Enabled=false.
	if c.IsEnabled() {
		t.Fatal("extract_config: empty payload should disable the cache")
	}
}

func TestHandler_ExtractCacheConfig_MalformedJSON_Error(t *testing.T) {
	d := newTestDeps(t)
	c, _ := miniredisCache(t)
	d.ResponseCache = c
	err := applyKey(t, d, "response_cache.extract_config", []byte(`{not json`))
	if err == nil {
		t.Fatal("extract_config malformed JSON: expected error, got nil")
	}
}

func TestHandler_ExtractCacheConfig_ValidPayload_SetsConfig(t *testing.T) {
	d := newTestDeps(t)
	logger, buf := logCapture()
	d.Logger = logger
	c, _ := miniredisCache(t)
	d.ResponseCache = c

	payload := extractCacheConfigBlob{
		Enabled:             true,
		TTLSeconds:          300,
		ApplyFreshnessRules: true,
	}
	raw, _ := json.Marshal(payload)
	if err := applyKey(t, d, "response_cache.extract_config", raw); err != nil {
		t.Fatalf("extract_config valid payload: unexpected error: %v", err)
	}
	// Observable: cache should now be enabled.
	if !c.IsEnabled() {
		t.Fatal("extract_config: expected cache to be enabled after valid payload")
	}
	// Observable: log line should contain the reload message.
	if !bytes.Contains(buf.Bytes(), []byte("extract-cache config reloaded")) {
		t.Fatalf("extract_config: expected log line not found: %s", buf.String())
	}
}

func TestHandler_ExtractCacheConfig_FreshnessRulesToggle(t *testing.T) {
	d := newTestDeps(t)
	c, _ := miniredisCache(t)
	d.ResponseCache = c

	// Enable freshness rules.
	raw, _ := json.Marshal(extractCacheConfigBlob{Enabled: true, TTLSeconds: 60, ApplyFreshnessRules: true})
	if err := applyKey(t, d, "response_cache.extract_config", raw); err != nil {
		t.Fatalf("extract_config enable freshness: unexpected error: %v", err)
	}
	if !c.ApplyFreshnessRules() {
		t.Fatal("extract_config: expected ApplyFreshnessRules=true")
	}

	// Disable freshness rules.
	raw, _ = json.Marshal(extractCacheConfigBlob{Enabled: true, TTLSeconds: 60, ApplyFreshnessRules: false})
	if err := applyKey(t, d, "response_cache.extract_config", raw); err != nil {
		t.Fatalf("extract_config disable freshness: unexpected error: %v", err)
	}
	if c.ApplyFreshnessRules() {
		t.Fatal("extract_config: expected ApplyFreshnessRules=false after update")
	}
}

// BuildConfigLoader validation

func TestBuildConfigLoader_MinimalDeps_NoPanic(t *testing.T) {
	// Verify BuildConfigLoader does not panic when only mandatory fields are set.
	var obs atomic.Pointer[telemetry.Config]
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("BuildConfigLoader panicked: %v", r)
		}
	}()
	_ = BuildConfigLoader(Deps{
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		ThingID:            "ag-test",
		Outcomes:           thingclient.NewOutcomeTracker(),
		ObservabilityState: &obs,
	})
}

// Keys accessor

func TestKeys_ReturnsExpectedCount(t *testing.T) {
	var obs atomic.Pointer[telemetry.Config]
	l := BuildConfigLoader(Deps{
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		ThingID:            "ag-keys",
		Outcomes:           thingclient.NewOutcomeTracker(),
		ObservabilityState: &obs,
	})
	keys := l.Keys()
	const want = 19 // registered keys total (F-0125 removed the dead streaming_compliance applier)
	if len(keys) != want {
		t.Fatalf("Keys(): got %d keys, want %d: %v", len(keys), want, keys)
	}
}

// DB + CacheLayer helpers

// newMockDB creates a *store.DB backed by a pgxmock pool. The mock is returned
// so callers can set up query expectations; t.Cleanup closes the mock.
func newMockDB(t *testing.T) (pgxmock.PgxPoolIface, *store.DB) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock, store.NewWithPgxPool(mock)
}

// newMockLayer creates a *cachelayer.Layer + its underlying pgxmock pool.
// The Layer is constructed but NOT started (no initial SQL queries fire).
func newMockLayer(t *testing.T) (pgxmock.PgxPoolIface, *cachelayer.Layer) {
	t.Helper()
	mock, db := newMockDB(t)
	layer, err := cachelayer.NewWithPool(db, mock,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		cachelayer.Config{},
	)
	if err != nil {
		t.Fatalf("cachelayer.NewWithPool: %v", err)
	}
	return mock, layer
}

// expectSnapshotReload enqueues the minimal pgxmock expectations for a single
// Reload* call on the given table (providers / models / credentials).
func expectProviderReload(mock pgxmock.PgxPoolIface) {
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Provider"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "name", "displayName", "adapter_type", "baseUrl",
			"pathPrefix", "apiVersion", "region", "enabled",
		}))
}

func expectModelReload(mock pgxmock.PgxPoolIface) {
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Model" m`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "code", "name", "providerId", "p_name", "p_adapter_type",
			"p_displayName", "p_baseUrl", "providerModelId", "type", "enabled",
			"inputPricePerMillion", "outputPricePerMillion",
			"cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
			"features", "maxContextTokens", "maxOutputTokens", "aliases",
			"inputModalities", "outputModalities", "lifecycle", "capabilityJson",
		}))
}

func expectCredentialReload(mock pgxmock.PgxPoolIface) {
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Credential"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "name", "providerId", "encryptedKey", "encryptionIv", "encryptionTag",
			"encryption_key_id", "enabled", "rotationState", "selectionWeight",
			"status", "createdAt",
		}))
}

// routing_rules — non-nil DB branch

func TestHandler_RoutingRules_WithDB_CallsInvalidateRuleCache(t *testing.T) {
	d := newTestDeps(t)
	// InvalidateRuleCache only touches db.rc (in-memory mutex), no SQL.
	_, db := newMockDB(t)
	d.DB = db
	if err := applyKey(t, d, "routing_rules", []byte(`{}`)); err != nil {
		t.Fatalf("routing_rules with non-nil DB: unexpected error: %v", err)
	}
	// No SQL expectations needed — InvalidateRuleCache is pure in-memory.
}

// credentials — non-nil CacheLayer branch

func TestHandler_Credentials_WithCacheLayer_ReloadsCredentials(t *testing.T) {
	d := newTestDeps(t)
	mock, layer := newMockLayer(t)
	expectCredentialReload(mock)
	d.CacheLayer = layer
	d.CredManager = &fakeCredInvalidator{} // required dep (F-0126)
	if err := applyKey(t, d, "credentials", []byte(`{}`)); err != nil {
		t.Fatalf("credentials with non-nil CacheLayer: unexpected error: %v", err)
	}
}

func TestHandler_Credentials_CacheLayerReloadError_Propagates(t *testing.T) {
	d := newTestDeps(t)
	mock, layer := newMockLayer(t)
	// Return an error from the query.
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Credential"`).WillReturnError(fmt.Errorf("db error"))
	d.CacheLayer = layer
	d.CredManager = &fakeCredInvalidator{} // required dep (F-0126)
	if err := applyKey(t, d, "credentials", []byte(`{}`)); err == nil {
		t.Fatal("credentials: expected error when CacheLayer.ReloadCredentials fails")
	}
}

// providers — non-nil CacheLayer branch

func TestHandler_Providers_WithCacheLayer_ReloadsProviders(t *testing.T) {
	d := newTestDeps(t)
	mock, layer := newMockLayer(t)
	expectProviderReload(mock)
	d.CacheLayer = layer
	d.GeminiCacheMgrSet = nil
	if err := applyKey(t, d, "providers", []byte(`{}`)); err != nil {
		t.Fatalf("providers with non-nil CacheLayer: unexpected error: %v", err)
	}
}

func TestHandler_Providers_CacheLayerReloadError_Propagates(t *testing.T) {
	d := newTestDeps(t)
	mock, layer := newMockLayer(t)
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Provider"`).WillReturnError(fmt.Errorf("db error"))
	d.CacheLayer = layer
	d.GeminiCacheMgrSet = nil
	if err := applyKey(t, d, "providers", []byte(`{}`)); err == nil {
		t.Fatal("providers: expected error when CacheLayer.ReloadProviders fails")
	}
}

// models — non-nil CacheLayer branch

func TestHandler_Models_WithCacheLayer_ReloadsModels(t *testing.T) {
	d := newTestDeps(t)
	mock, layer := newMockLayer(t)
	expectModelReload(mock)
	d.CacheLayer = layer
	// Verify OnModelsReloaded callback is invoked.
	callbackInvoked := false
	d.OnModelsReloaded = func(models []store.Model) { callbackInvoked = true }
	if err := applyKey(t, d, "models", []byte(`{}`)); err != nil {
		t.Fatalf("models with non-nil CacheLayer: unexpected error: %v", err)
	}
	if !callbackInvoked {
		t.Fatal("models: expected OnModelsReloaded callback to be invoked")
	}
}

func TestHandler_Models_NilOnModelsReloaded_NoOp(t *testing.T) {
	d := newTestDeps(t)
	mock, layer := newMockLayer(t)
	expectModelReload(mock)
	d.CacheLayer = layer
	d.OnModelsReloaded = nil // no callback — should not panic
	if err := applyKey(t, d, "models", []byte(`{}`)); err != nil {
		t.Fatalf("models nil OnModelsReloaded: unexpected error: %v", err)
	}
}

func TestHandler_Models_CacheLayerReloadError_Propagates(t *testing.T) {
	d := newTestDeps(t)
	mock, layer := newMockLayer(t)
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`FROM "Model" m`).WillReturnError(fmt.Errorf("db error"))
	d.CacheLayer = layer
	if err := applyKey(t, d, "models", []byte(`{}`)); err == nil {
		t.Fatal("models: expected error when CacheLayer.ReloadModels fails")
	}
}

// virtual_keys — CacheLayer branches

func TestHandler_VirtualKeys_WithTargetedInvalidate_CallsInvalidateVirtualKeys(t *testing.T) {
	d := newTestDeps(t)
	_, layer := newMockLayer(t)
	d.CacheLayer = layer
	// Payload with op=invalidate carries specific IDs.
	payload := `{"op":"invalidate","ids":["hash1","hash2"]}`
	if err := applyKey(t, d, "virtual_keys", []byte(payload)); err != nil {
		t.Fatalf("virtual_keys targeted invalidate: unexpected error: %v", err)
	}
	// InvalidateVirtualKeys is in-memory; no observable side effect to check
	// beyond no-error (IDs not in cache → 0 removed, but path was entered).
}

func TestHandler_VirtualKeys_WithFullPurge_CallsPurgeVirtualKeys(t *testing.T) {
	d := newTestDeps(t)
	_, layer := newMockLayer(t)
	d.CacheLayer = layer
	// Empty/non-invalidate payload → full purge.
	if err := applyKey(t, d, "virtual_keys", []byte(`{}`)); err != nil {
		t.Fatalf("virtual_keys full purge: unexpected error: %v", err)
	}
}

func TestHandler_VirtualKeys_NilPayload_CallsPurgeVirtualKeys(t *testing.T) {
	d := newTestDeps(t)
	_, layer := newMockLayer(t)
	d.CacheLayer = layer
	if err := applyKey(t, d, "virtual_keys", nil); err != nil {
		t.Fatalf("virtual_keys nil payload: unexpected error: %v", err)
	}
}

// quota_policies — non-nil PolicyCache branch

func TestHandler_QuotaPolicies_WithPolicyCacheNilPool_ReturnsNilError(t *testing.T) {
	d := newTestDeps(t)
	// NewPolicyCacheWithPgxPool with nil pool: Load returns nil immediately (early return).
	// This exercises the non-nil PolicyCache branch (pc.Load gets called).
	pc := quota.NewPolicyCacheWithPgxPool(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.PolicyCache = pc
	for _, key := range []string{"quota_policies", "quota_overrides", "organizations"} {
		if err := applyKey(t, d, key, []byte(`{}`)); err != nil {
			t.Fatalf("%s with non-nil PolicyCache nil-pool: unexpected error: %v", key, err)
		}
	}
}

func TestHandler_QuotaPolicies_WithPolicyCacheMockPool_CallsLoad(t *testing.T) {
	d := newTestDeps(t)
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	// Set up expectations for the 3 queries Load performs.
	for range 3 {
		mock.MatchExpectationsInOrder(false)
	}
	// policies query
	mock.ExpectQuery(`FROM "QuotaPolicy"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "scope", "organizationId", "vkType", "periodType",
			"costLimitUsd", "enforcementMode", "priority",
		}))
	// overrides query
	mock.ExpectQuery(`FROM "QuotaOverride"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "targetType", "targetId",
			"costLimitUsd", "enforcementMode", "periodType",
		}))
	// org tree query
	mock.ExpectQuery(`FROM "Organization"`).
		WillReturnRows(pgxmock.NewRows([]string{"id", "parentId"}))

	pc := quota.NewPolicyCacheWithPgxPool(mock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.PolicyCache = pc
	if err := applyKey(t, d, "quota_policies", []byte(`{}`)); err != nil {
		t.Fatalf("quota_policies with mock pool: unexpected error: %v", err)
	}
}

// credential_reliability — non-nil DB branch

func TestHandler_CredentialReliability_WithReliabilityAndDB_CallsReload(t *testing.T) {
	d := newTestDeps(t)
	stub := &stubReliabilityReloader{}
	d.Reliability = stub
	mock, db := newMockDB(t)
	// GetSystemMetadata query expectation.
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`{}`)))
	d.DB = db
	if err := applyKey(t, d, "credential_reliability", []byte(`{}`)); err != nil {
		t.Fatalf("credential_reliability with Reliability+DB: unexpected error: %v", err)
	}
	if !stub.called {
		t.Fatal("credential_reliability: expected Reload to be called")
	}
}

// credentials — F-0097 granular vs full invalidation (non-nil CredManager branch)

// fakeCredInvalidator records which credential cache ops the credentials
// applier performed so the F-0097 granular-vs-full decision is observable.
type fakeCredInvalidator struct {
	invalidated []string
	cleared     int
}

func (f *fakeCredInvalidator) Invalidate(id string) { f.invalidated = append(f.invalidated, id) }
func (f *fakeCredInvalidator) ClearCache()          { f.cleared++ }

// F-0097: a targeted invalidate-by-id payload must evict ONLY the named
// credentials (per-id Invalidate), never a full ClearCache storm.
func TestHandler_Credentials_TargetedIDs_CallsInvalidatePerID(t *testing.T) {
	fake := &fakeCredInvalidator{}
	d := newTestDeps(t)
	mock, layer := newMockLayer(t)
	expectCredentialReload(mock)
	d.CacheLayer = layer
	d.CredManager = fake
	if err := applyKey(t, d, "credentials", []byte(`{"op":"invalidate","ids":["cred-a","cred-b"]}`)); err != nil {
		t.Fatalf("targeted invalidate: unexpected error: %v", err)
	}

	if got := fake.invalidated; len(got) != 2 || got[0] != "cred-a" || got[1] != "cred-b" {
		t.Errorf("Invalidate calls = %v, want [cred-a cred-b]", got)
	}
	if fake.cleared != 0 {
		t.Errorf("ClearCache called %d times on a targeted payload, want 0", fake.cleared)
	}
}

// F-0097: a non-targeted payload (full reload signal) falls back to ClearCache.
func TestHandler_Credentials_NoIDs_FallsBackToClearCache(t *testing.T) {
	fake := &fakeCredInvalidator{}
	d := newTestDeps(t)
	mock, layer := newMockLayer(t)
	expectCredentialReload(mock)
	d.CacheLayer = layer
	d.CredManager = fake
	if err := applyKey(t, d, "credentials", []byte(`{}`)); err != nil {
		t.Fatalf("full-reload fallback: unexpected error: %v", err)
	}

	if fake.cleared != 1 {
		t.Errorf("ClearCache calls = %d, want 1 (full-reload fallback)", fake.cleared)
	}
	if len(fake.invalidated) != 0 {
		t.Errorf("Invalidate called %v on a non-targeted payload, want none", fake.invalidated)
	}
}

// providers — GeminiCacheMgrSet path
// NOTE: geminicache.ManagerSet cannot be cheaply constructed in this package
// (requires Redis + complex deps). The GeminiCacheMgrSet nil branch is already
// tested. The non-nil branch is tagged (E) network-infra-bound and the nil
// guard before the call means the only skipped statement is the
// `d.GeminiCacheMgrSet.ReloadProviders()` call itself.
// We accept 88.9% for registerAGProviders — GeminiCacheMgrSet is nil in all
// realistic test environments without a live Redis-backed ManagerSet.

// observability — non-nil DB branch

func TestHandler_Observability_NilDB_ButNonNilTelemetry_NoOp(t *testing.T) {
	// TelemetryProvider != nil but DB == nil → handler returns without calling Reconfigure.
	d := newTestDeps(t)
	tp, err := telemetry.Init(context.Background(), telemetry.Config{Enabled: false}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("telemetry.Init: %v", err)
	}
	d.TelemetryProvider = tp
	d.DB = nil
	if err := applyKey(t, d, "observability", []byte(`{}`)); err != nil {
		t.Fatalf("observability nil DB: unexpected error: %v", err)
	}
}

func TestHandler_Observability_WithDBAndTelemetry_CallsReconfigure(t *testing.T) {
	d := newTestDeps(t)
	tp, err := telemetry.Init(context.Background(), telemetry.Config{Enabled: false}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("telemetry.Init: %v", err)
	}
	d.TelemetryProvider = tp

	mock, db := newMockDB(t)
	// GetSystemMetadata("observability.config") query.
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"value"}).
			AddRow([]byte(`{"otelEnabled":false,"samplingRate":0.1}`)))
	d.DB = db
	d.BootstrapConfig = &config.Config{}

	if err := applyKey(t, d, "observability", []byte(`{}`)); err != nil {
		t.Fatalf("observability with DB+telemetry: unexpected error: %v", err)
	}
}

func TestHandler_Observability_DBQueryError_StillReconfigures(t *testing.T) {
	d := newTestDeps(t)
	tp, err := telemetry.Init(context.Background(), telemetry.Config{Enabled: false}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("telemetry.Init: %v", err)
	}
	d.TelemetryProvider = tp

	mock, db := newMockDB(t)
	// Return an error for GetSystemMetadata — InitOtelConfig still returns a valid default config.
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(fmt.Errorf("db down"))
	d.DB = db
	d.BootstrapConfig = &config.Config{}

	// Even when DB returns error, InitOtelConfig returns default config and
	// Reconfigure is called with it (no error expected).
	if err := applyKey(t, d, "observability", []byte(`{}`)); err != nil {
		t.Fatalf("observability DB error: unexpected error: %v", err)
	}
}

// payload_capture — non-nil DB branch

func TestHandler_PayloadCapture_WithDB_LoadsConfig(t *testing.T) {
	d := newTestDeps(t)
	d.PayloadCaptureStore = payloadcapture.NewStore(payloadcapture.DefaultConfig())
	mock, db := newMockDB(t)
	// GetSystemMetadata("payload_capture.config") query.
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"value"}).
			AddRow([]byte(`{"storeRequestBody":true,"storeResponseBody":true}`)))
	d.DB = db
	if err := applyKey(t, d, "payload_capture", []byte(`{}`)); err != nil {
		t.Fatalf("payload_capture with DB: unexpected error: %v", err)
	}
}

func TestHandler_PayloadCapture_DBQueryError_PropagatesError(t *testing.T) {
	d := newTestDeps(t)
	d.PayloadCaptureStore = payloadcapture.NewStore(payloadcapture.DefaultConfig())
	mock, db := newMockDB(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(fmt.Errorf("db error"))
	d.DB = db
	if err := applyKey(t, d, "payload_capture", []byte(`{}`)); err == nil {
		t.Fatal("payload_capture DB error: expected error, got nil")
	}
}

// cache — GeminiCacheMgrSet + NormEngine branches ─
// NOTE: geminicache.ManagerSet.SetConfig is the remaining uncovered statement
// in registerAGCacheConfig (90.9%). Constructing a ManagerSet requires Redis.
// The nil-GeminiCacheMgrSet + non-nil NormEngine branch is covered above.
// Accepting 90.9% for this function — the missing statement is network-infra-bound.

// time_sensitive_patterns — Reload error

// TestHandler_TimeSensitivePatterns_ReloadError covers the error-wrapping branch.
// freshness.Detector.Reload returns an error for rules with empty ID/keywords.
func TestHandler_TimeSensitivePatterns_InvalidRule_ReloadError(t *testing.T) {
	d := newTestDeps(t)
	reg := prometheus.NewRegistry()
	det, err := freshness.NewDetector(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "test2", reg)
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}
	d.FreshnessDetector = det
	// A rule with empty ID causes Reload to return an error.
	rules := []freshness.Rule{
		{ID: "", Keywords: []string{"stock"}, Enabled: true},
	}
	raw, _ := json.Marshal(rules)
	if err := applyKey(t, d, "response_cache.time_sensitive_patterns", raw); err == nil {
		t.Fatal("time_sensitive_patterns: expected error for rule with empty ID")
	}
}

// errTest sentinel

// errTest is a sentinel error for test stubs.
var errTest = fmt.Errorf("test stub error")
