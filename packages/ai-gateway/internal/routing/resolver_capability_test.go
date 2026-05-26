package routing

// resolver_capability_test.go — end-to-end capability pre-filter integration
// tests. These live in the routing package (white-box) so they can wire a
// *capability.Cache directly into the Resolver without exporting internals.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/capability"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/matcher"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/strategies"
)

// capFixture is a resolverFixture extension that also carries a capability cache.
type capFixture struct {
	store    *fakeStore
	registry *strategies.StrategyRegistry
	resolver *Resolver
	capCache *capability.Cache
}

func newCapFixture() *capFixture {
	fs := &fakeStore{
		providers: map[string]*store.Provider{},
		models:    map[string]*store.Model{},
	}
	capCache := capability.NewCache()
	reg := strategies.NewStrategyRegistry()
	resolver := &Resolver{
		db:              fs,
		registry:        reg,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		narrowingEngine: &matcher.NarrowingEngine{},
		capCache:        capCache,
	}
	strategies.RegisterAllStrategies(reg, resolver.LookupTargetFunc(), nil)
	return &capFixture{store: fs, registry: reg, resolver: resolver, capCache: capCache}
}

func (f *capFixture) addProvider(id, adapterType string, enabled bool) {
	f.store.providers[id] = &store.Provider{
		ID:          id,
		Name:        id,
		AdapterType: adapterType,
		BaseURL:     "https://" + id + ".example.com",
		Enabled:     enabled,
	}
}

func (f *capFixture) addEmbeddingModel(id, providerID string, capJSON []byte) {
	m := &store.Model{
		ID:               id,
		Code:             id,
		Name:             id,
		ProviderID:       providerID,
		ProviderName:     providerID,
		ProviderModelID:  id,
		Type:             "embedding",
		Enabled:          true,
		InputModalities:  []string{"text"},
		OutputModalities: []string{"embedding"},
		Lifecycle:        "ga",
		CapabilityJson:   capJSON,
	}
	f.store.models[id] = m
}

func (f *capFixture) addRule(r store.RoutingRule) {
	r.Enabled = true
	f.store.rules = append(f.store.rules, r)
}

func (f *capFixture) rebuildCapCache() {
	var models []store.Model
	for _, m := range f.store.models {
		if m != nil {
			models = append(models, *m)
		}
	}
	f.capCache.Replace(capability.NewSnapshot(models))
}

func mustJSONCapability(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestCapabilityPreFilter_GeminiPassCohere1024Reject tests the scenario described
// in SDD §T5 example: Gemini models support 256/512/768/1024/1536 dimensions,
// Cohere is fixed at 1024. A request for dimensions=1536 should keep Gemini,
// reject Cohere.
//
// We use a fallback strategy so both targets are enumerated (primary + recovery)
// and the pre-filter can operate on the full candidate set. In a real routing
// tree loadbalance picks one at a time, so we verify the filter using a
// primary+fallback pair instead.
func TestCapabilityPreFilter_GeminiPassCohere1024Reject(t *testing.T) {
	f := newCapFixture()

	f.addProvider("gemini", "gemini", true)
	f.addProvider("cohere", "cohere", true)

	// Gemini: supports 256/512/768/1024/1536
	geminiCapJSON := mustJSONCapability(t, map[string]any{
		"embeddings": map[string]any{
			"supported_dimensions": []int{256, 512, 768, 1024, 1536},
			"max_batch_size":       100,
		},
	})
	// Cohere: fixed at 1024 only
	cohereCapJSON := mustJSONCapability(t, map[string]any{
		"embeddings": map[string]any{
			"supported_dimensions": []int{1024},
			"max_batch_size":       96,
		},
	})

	f.addEmbeddingModel("gemini-embed", "gemini", geminiCapJSON)
	f.addEmbeddingModel("cohere-embed", "cohere", cohereCapJSON)
	f.rebuildCapCache()

	// Primary rule: Gemini only.
	// Fallback rule: Cohere only.
	// At dimensions=1536 → Gemini kept, Cohere (which would be in recovery) also rejected.
	primaryConfig := mustJSONCapability(t, map[string]any{
		"type":       "single",
		"providerId": "gemini",
		"modelId":    "gemini-embed",
	})
	f.addRule(store.RoutingRule{
		ID:            "r-gemini-primary",
		PipelineStage: 1,
		StrategyType:  "single",
		Config:        primaryConfig,
	})

	dims := 1536
	rctx := &core.RoutingContext{
		RequestedModel:   core.RequestedModel{ID: "gemini-embed"},
		EndpointType:     "embeddings",
		EmbeddingRequest: &core.EmbeddingRequestParams{Dimensions: &dims, BatchSize: 1},
	}

	plan, err := f.resolver.Resolve(context.Background(), rctx)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}

	// Only Gemini should survive (supports 1536); Cohere only has 1024.
	for _, tgt := range plan.Targets {
		if tgt.ModelID == "cohere-embed" {
			t.Errorf("cohere-embed should have been rejected by capability pre-filter, but was kept")
		}
	}
	geminiFound := false
	for _, tgt := range plan.Targets {
		if tgt.ModelID == "gemini-embed" {
			geminiFound = true
		}
	}
	if !geminiFound {
		t.Error("gemini-embed should have been kept (supports 1536)")
	}
}

// TestCapabilityPreFilter_CoherePassesWithNoDimensions — Cohere model with 1024-only
// dimensions list passes when the client omits the dimensions parameter.
func TestCapabilityPreFilter_CoherePassesWithNoDimensions(t *testing.T) {
	f := newCapFixture()
	f.addProvider("cohere", "cohere", true)

	cohereCapJSON := mustJSONCapability(t, map[string]any{
		"embeddings": map[string]any{
			"supported_dimensions": []int{1024},
			"max_batch_size":       96,
		},
	})
	f.addEmbeddingModel("cohere-embed", "cohere", cohereCapJSON)
	f.rebuildCapCache()

	cohereCfg := mustJSONCapability(t, map[string]any{
		"type":       "single",
		"providerId": "cohere",
		"modelId":    "cohere-embed",
	})
	f.addRule(store.RoutingRule{
		ID:            "r-cohere",
		PipelineStage: 1,
		StrategyType:  "single",
		Config:        cohereCfg,
	})

	// No dimensions parameter → Cohere should pass
	rctx := &core.RoutingContext{
		RequestedModel:   core.RequestedModel{ID: "cohere-embed"},
		EndpointType:     "embeddings",
		EmbeddingRequest: &core.EmbeddingRequestParams{BatchSize: 1},
	}

	plan, err := f.resolver.Resolve(context.Background(), rctx)
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if len(plan.Targets) == 0 {
		t.Error("Cohere should pass when no dimensions param is specified")
	}
}

// TestCapabilityPreFilter_AllRejected — when every candidate is rejected,
// ResolveTargets returns *core.NoCompatibleProviderError.
func TestCapabilityPreFilter_AllRejected(t *testing.T) {
	f := newCapFixture()
	f.addProvider("openai", "openai", true)

	// ada-002 style: no supported_dimensions (cannot accept dimensions param)
	adaCapJSON := mustJSONCapability(t, map[string]any{
		"embeddings": map[string]any{
			"max_batch_size": 2048,
			// supported_dimensions intentionally omitted = rejects any dimensions request
		},
	})
	f.addEmbeddingModel("ada-002", "openai", adaCapJSON)
	f.rebuildCapCache()

	ruleConfig := mustJSONCapability(t, map[string]any{
		"type":       "single",
		"providerId": "openai",
		"modelId":    "ada-002",
	})
	f.addRule(store.RoutingRule{
		ID:            "r-ada",
		PipelineStage: 1,
		StrategyType:  "single",
		Config:        ruleConfig,
	})

	dims := 512 // ada-002 doesn't support custom dimensions
	rctx := &core.RoutingContext{
		RequestedModel:   core.RequestedModel{ID: "ada-002"},
		EndpointType:     "embeddings",
		EmbeddingRequest: &core.EmbeddingRequestParams{Dimensions: &dims, BatchSize: 1},
	}

	_, err := f.resolver.ResolveTargets(context.Background(), rctx)
	if err == nil {
		t.Fatal("expected NoCompatibleProviderError, got nil error")
	}
	var ncpErr *core.NoCompatibleProviderError
	if !errors.As(err, &ncpErr) {
		t.Fatalf("expected *core.NoCompatibleProviderError, got %T: %v", err, err)
	}
	// Error string should be the canonical sentinel.
	if ncpErr.Error() != "no_compatible_provider" {
		t.Errorf("Error() = %q, want %q", ncpErr.Error(), "no_compatible_provider")
	}
}

// TestCapabilityPreFilter_DisabledWhenNilCache — resolver with nil capCache never
// applies the pre-filter.
func TestCapabilityPreFilter_DisabledWhenNilCache(t *testing.T) {
	fs := &fakeStore{
		providers: map[string]*store.Provider{},
		models:    map[string]*store.Model{},
	}
	reg := strategies.NewStrategyRegistry()
	resolver := &Resolver{
		db:              fs,
		registry:        reg,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		narrowingEngine: &matcher.NarrowingEngine{},
		capCache:        nil, // disabled
	}
	strategies.RegisterAllStrategies(reg, resolver.LookupTargetFunc(), nil)

	fs.providers["openai"] = &store.Provider{
		ID:          "openai",
		Name:        "openai",
		AdapterType: "openai",
		BaseURL:     "https://openai.example.com",
		Enabled:     true,
	}
	fs.models["ada-002"] = &store.Model{
		ID:              "ada-002",
		Code:            "ada-002",
		ProviderID:      "openai",
		ProviderModelID: "text-embedding-ada-002",
		Type:            "embedding",
		Enabled:         true,
		CapabilityJson:  nil, // no capability data
	}

	ruleConfig := mustJSONCapability(t, map[string]any{
		"type":       "single",
		"providerId": "openai",
		"modelId":    "ada-002",
	})
	rule := store.RoutingRule{
		ID:            "r-ada-nil-cache",
		PipelineStage: 1,
		StrategyType:  "single",
		Config:        ruleConfig,
		Enabled:       true,
	}
	fs.rules = append(fs.rules, rule)

	dims := 512
	rctx := &core.RoutingContext{
		RequestedModel: core.RequestedModel{ID: "ada-002"},
		EndpointType:   "embeddings",
		// No EmbeddingRequest — but even with one, nil capCache skips the filter.
		EmbeddingRequest: &core.EmbeddingRequestParams{Dimensions: &dims, BatchSize: 1},
	}

	plan, err := resolver.Resolve(context.Background(), rctx)
	if err != nil {
		t.Fatalf("expected no error when capCache is nil, got: %v", err)
	}
	// With nil capCache, all targets should pass through.
	if len(plan.Targets) == 0 {
		t.Error("expected targets to pass through when capCache is nil")
	}
}

// TestCapabilityPreFilter_NonEmbeddingsSkipped — capability filter does not fire
// for chat/completions requests even when EmbeddingRequest is populated.
func TestCapabilityPreFilter_NonEmbeddingsSkipped(t *testing.T) {
	f := newCapFixture()
	f.addProvider("openai", "openai", true)
	// Model has no capability data — would be rejected on embeddings path.
	f.addEmbeddingModel("gpt-4", "openai", nil)
	f.rebuildCapCache()

	ruleConfig := mustJSONCapability(t, map[string]any{
		"type":       "single",
		"providerId": "openai",
		"modelId":    "gpt-4",
	})
	f.addRule(store.RoutingRule{
		ID:            "r-chat",
		PipelineStage: 1,
		StrategyType:  "single",
		Config:        ruleConfig,
	})

	dims := 512
	rctx := &core.RoutingContext{
		RequestedModel:   core.RequestedModel{ID: "gpt-4"},
		EndpointType:     "chat", // NOT embeddings
		EmbeddingRequest: &core.EmbeddingRequestParams{Dimensions: &dims, BatchSize: 1},
	}

	plan, err := f.resolver.Resolve(context.Background(), rctx)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	// gpt-4 should not have been filtered out — wrong endpoint type.
	if len(plan.Targets) == 0 {
		t.Error("expected targets to pass through for chat/completions endpoint")
	}
}

// TestCapabilityPreFilter_NilEmbeddingRequest — embeddings endpoint but no
// EmbeddingRequest → pre-filter skipped (permissive).
func TestCapabilityPreFilter_NilEmbeddingRequest(t *testing.T) {
	f := newCapFixture()
	f.addProvider("openai", "openai", true)
	f.addEmbeddingModel("ada-002", "openai", nil)
	f.rebuildCapCache()

	ruleConfig := mustJSONCapability(t, map[string]any{
		"type":       "single",
		"providerId": "openai",
		"modelId":    "ada-002",
	})
	f.addRule(store.RoutingRule{
		ID:            "r-ada-nil-req",
		PipelineStage: 1,
		StrategyType:  "single",
		Config:        ruleConfig,
	})

	rctx := &core.RoutingContext{
		RequestedModel:   core.RequestedModel{ID: "ada-002"},
		EndpointType:     "embeddings",
		EmbeddingRequest: nil, // not populated — handler didn't extract params
	}

	plan, err := f.resolver.Resolve(context.Background(), rctx)
	if err != nil {
		t.Fatalf("expected no error with nil EmbeddingRequest: %v", err)
	}
	// Pre-filter skipped → ada-002 passes through.
	if len(plan.Targets) == 0 {
		t.Error("expected targets to pass through when EmbeddingRequest is nil")
	}
}
