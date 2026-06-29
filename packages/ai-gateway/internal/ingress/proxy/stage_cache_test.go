// stage_cache_test.go — characterization pins for the cache stage of
// the proxy pipeline: skip decisions (client no-cache header, operator
// passthrough bypass, missing adapter) and the cross-format MISS
// canonicalization that prepares the upstream wire body.
package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/passthrough"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
	"github.com/tidwall/gjson"
)

// seedNonStreamCacheEntry stores a ResponseEntry under the same key the
// handler derives for the supplied openai/gpt-4o body so a follow-up
// request can HIT it.
func seedNonStreamCacheEntry(t *testing.T, deps *Deps, body, cachedResp []byte) {
	t.Helper()
	in, out, total := 3, 4, 7
	key := deps.Cache.BuildKey("openai", "gpt-4o", body, "")
	if _, err := deps.Cache.StoreResponse(context.Background(), key, &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: cachedResp,
		Usage:             provcore.Usage{PromptTokens: &in, CompletionTokens: &out, TotalTokens: &total},
		CachedAt:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
}

// TestServeProxy_NoCacheHeader_BypassesSeededCacheEntry pins the client
// opt-out: with a hittable entry seeded, `x-nexus-aigw-no-cache` forces
// the live upstream (X-Nexus-Cache: MISS); the same request without the
// header replays the cached response (HIT) — proving the skip decision,
// not a key mismatch, produced the MISS.
func TestServeProxy_NoCacheHeader_BypassesSeededCacheEntry(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	upstream := openAIChatUpstream(t, `{
		"id":"live","object":"chat.completion","model":"gpt-4o",
		"choices":[{"index":0,"message":{"role":"assistant","content":"live-hello"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
	}`)
	defer upstream.Close()

	deps := makeOpenAIDeps(t, upstream.URL, emptyHookCache(t), cacheOpt)
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi cache"}]}`)
	cachedResp := []byte(`{"id":"cached-1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"cached-hello"},"finish_reason":"stop"}]}`)
	seedNonStreamCacheEntry(t, deps, body, cachedResp)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})

	// With the no-cache header: live upstream despite the seeded entry.
	req := freshChatRequest(t, string(body))
	req.Header.Set("x-nexus-aigw-no-cache", "1")
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "live-hello") {
		t.Errorf("body=%s want live upstream response (cache skipped)", w.Body.String())
	}
	if got := w.Header().Get("X-Nexus-Cache"); got != "MISS" {
		t.Errorf("X-Nexus-Cache=%q want MISS on the skip path", got)
	}

	// Without the header: the seeded entry is served — proving it was hittable.
	w2 := httptest.NewRecorder()
	h(w2, freshChatRequest(t, string(body)))
	if w2.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "cached-hello") {
		t.Errorf("body=%s want cached response without the header", w2.Body.String())
	}
	if got := w2.Header().Get("X-Nexus-Cache"); got != "HIT" {
		t.Errorf("X-Nexus-Cache=%q want HIT", got)
	}
}

// TestServeProxy_PassthroughBypassCache_BypassesSeededCacheEntry pins the
// operator bypass: an active passthrough config with bypassCache forces
// the live upstream even when a hittable entry exists; clearing the
// snapshot restores cache HITs on the very next request.
func TestServeProxy_PassthroughBypassCache_BypassesSeededCacheEntry(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	upstream := openAIChatUpstream(t, `{
		"id":"live","object":"chat.completion","model":"gpt-4o",
		"choices":[{"index":0,"message":{"role":"assistant","content":"live-hello"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
	}`)
	defer upstream.Close()

	pcache := passthrough.NewCache()
	future := time.Now().Add(1 * time.Hour)
	pcache.SetSnapshot(&passthrough.Snapshot{
		Global: passthrough.TierEntry{
			Enabled:     true,
			BypassCache: true,
			ExpiresAt:   &future,
			EnabledBy:   "test",
			Reason:      "test bypass cache",
		},
	})

	deps := makeOpenAIDeps(t, upstream.URL, emptyHookCache(t), cacheOpt, func(d *Deps) {
		d.PassthroughCache = pcache
	})
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi cache"}]}`)
	cachedResp := []byte(`{"id":"cached-1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"cached-hello"},"finish_reason":"stop"}]}`)
	seedNonStreamCacheEntry(t, deps, body, cachedResp)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})

	// Bypass active: live upstream despite the seeded entry.
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, string(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "live-hello") {
		t.Errorf("body=%s want live upstream response (passthrough bypassed cache)", w.Body.String())
	}
	if got := w.Header().Get("X-Nexus-Cache"); got != "MISS" {
		t.Errorf("X-Nexus-Cache=%q want MISS while bypass is active", got)
	}

	// Bypass cleared: the seeded entry is served again.
	pcache.SetSnapshot(&passthrough.Snapshot{})
	w2 := httptest.NewRecorder()
	h(w2, freshChatRequest(t, string(body)))
	if !strings.Contains(w2.Body.String(), "cached-hello") {
		t.Errorf("body=%s want cached response after bypass cleared", w2.Body.String())
	}
}

// TestServeProxy_CacheEnabled_MissingAdapter_FallsBackToLiveUpstream pins
// the defensive skip: cache enabled but no adapter registered for the
// routed target's format — the request must skip cache preparation and
// still be served by the executor instead of failing.
func TestServeProxy_CacheEnabled_MissingAdapter_FallsBackToLiveUpstream(t *testing.T) {
	respBody := []byte(`{
		"id":"x","object":"chat.completion","model":"gpt-4o",
		"choices":[{"index":0,"message":{"role":"assistant","content":"live-direct"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
	}`)
	hdrs := http.Header{}
	hdrs.Set("Content-Type", "application/json")
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    hdrs,
		Body:       respBody,
		Target:     routingcore.RoutingTarget{ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", ModelCode: "gpt-4o", AdapterType: "openai"},
		Attempts:   []executor.Attempt{{StatusCode: http.StatusOK}},
	}}
	fbridge := &fakeBridge{}
	deps := makeFakeDeps(t, fexec, fbridge)
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	cacheOpt(deps)
	emptyReg := provcore.NewRegistry()
	emptyReg.Freeze()
	deps.ProviderReg = emptyReg

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (live fallback); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "live-direct") {
		t.Errorf("body=%s want executor-served response", w.Body.String())
	}
	if got := w.Header().Get("X-Nexus-Cache"); got != "MISS" {
		t.Errorf("X-Nexus-Cache=%q want MISS", got)
	}
	if fexec.Calls+fexec.PreparedCalls == 0 {
		t.Error("executor must be invoked on the live fallback")
	}
}

// TestServeProxy_Stream_CrossFormatMISS_CanonicalUpstreamBodyCarriesStreamFlag
// pins the cross-format MISS preparation on a streaming request: an
// Anthropic-ingress body routed to an OpenAI target is canonicalized,
// and the wire body dispatched upstream carries the streaming intent
// (`"stream":true`) plus the caller's message content.
func TestServeProxy_Stream_CrossFormatMISS_CanonicalUpstreamBodyCarriesStreamFlag(t *testing.T) {
	var mu sync.Mutex
	var upstreamGot []byte
	frames := []string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamGot = b
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			_, _ = w.Write([]byte(f + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	deps := makeOpenAIDeps(t, upstream.URL, emptyHookCache(t), cacheOpt)

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeAnthropicMessages,
		BodyFormat: provcore.FormatAnthropic,
		Stream:     true,
	})
	body := `{"model":"gpt-4o","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi cross"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Nexus-Cache"); got != "MISS" {
		t.Errorf("X-Nexus-Cache=%q want MISS", got)
	}
	mu.Lock()
	got := upstreamGot
	mu.Unlock()
	if !gjson.GetBytes(got, "stream").Bool() {
		t.Errorf("upstream body=%s want \"stream\":true on the canonicalized wire body", got)
	}
	if !strings.Contains(gjson.GetBytes(got, "messages").Raw, "hi cross") {
		t.Errorf("upstream body=%s want caller content preserved through canonicalization", got)
	}
}

// TestServeProxy_SemanticCacheSkip_StillInjectsProviderCacheMarkers (live
// incident: ~0% Anthropic prompt-cache on the assistant's own traffic): a
// request the SEMANTIC cache skips (client no-cache here; time-sensitive and
// agentic skips take the same path) must still get its provider-side
// cache_control markers — the two caches are independent optimizations, and
// skipping ours must never disable the provider's.
func TestServeProxy_SemanticCacheSkip_StillInjectsProviderCacheMarkers(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()

	// Anthropic-wire upstream that CAPTURES the body it receives.
	var gotBody []byte
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = b
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",
			"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":3,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	deps := makeOpenAIDeps(t, upstream.URL, emptyHookCache(t), cacheOpt, func(d *Deps) {
		// Route to an anthropic-adapter target so the canonicalize→PrepareBody
		// chain emits the Anthropic Messages wire (where markers inject).
		d.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
			ProviderID:      "p-anthropic",
			ProviderName:    "anthropic",
			ProviderModelID: "claude-x",
			ModelID:         "claude-x",
			ModelName:       "Claude X",
			ModelCode:       "claude-x",
			AdapterType:     "anthropic",
		}}}
		// The real injection engine, configured the way prod is (global
		// normaliser on + anthropic marker injection on).
		eng := wirerewrite.New(nil)
		eng.Reload(wirerewrite.Config{
			NormaliserEnabled: true,
			Providers: map[string]wirerewrite.ProviderCacheConfig{
				"p-anthropic": {CacheMarkerInjectEnabled: true},
			},
		})
		d.Normaliser = eng
	})

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})

	// A long system prompt (markers only inject on cache-worthy prefixes).
	body := `{"model":"claude-x","messages":[{"role":"system","content":"` + strings.Repeat("stable operator playbook. ", 400) + `"},{"role":"user","content":"hello"}]}`
	req := freshChatRequest(t, body)
	req.Header.Set("x-nexus-aigw-no-cache", "1") // → semantic-cache SKIP path
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(string(gotBody), `"cache_control"`) {
		t.Fatalf("the provider must receive cache_control markers even on the semantic-cache skip path; upstream got:\n%.600s", gotBody)
	}
}

// TestServeProxy_L1Disabled_L2Enabled_StageStaysActive proves the L1/L2
// decouple: with the L1 extract cache OFF (deps.Cache nil) but the L2 semantic
// tier ON, the cache stage must stay ACTIVE — skip the L1 lookup, attempt L2,
// and on an L2 miss fall through to the live upstream stamping X-Nexus-Cache:
// MISS. Before the decouple the stage gated the WHOLE pipeline on L1's enabled
// flag, so an L1-off deployment short-circuited as "disabled" and never reached
// L2 (no MISS stamp). The MISS header is therefore the differential: it is
// emitted only from the active-cache default branch, never from the disabled
// skip branch. (L2 actually serving a hit is covered by the tryL2Lookup tests.)
func TestServeProxy_L1Disabled_L2Enabled_StageStaysActive(t *testing.T) {
	upstream := openAIChatUpstream(t, `{
		"id":"live-l2miss","object":"chat.completion","model":"gpt-4o",
		"choices":[{"index":0,"message":{"role":"assistant","content":"live-after-l2-miss"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
	}`)
	defer upstream.Close()

	// L2 reader present but returns no entry (miss) → the stage must fall
	// through to upstream as a MISS, not skip as disabled.
	rdr := &stubSemanticReader{result: semantic.ReadResult{}}

	// No withCache → deps.Cache stays nil → L1 disabled. L2 enabled via the
	// stub reader + an enabled fleet semantic config.
	deps := makeOpenAIDeps(t, upstream.URL, emptyHookCache(t), func(d *Deps) {
		d.SemanticReader = rdr
		d.SemanticConfigCache = enabledFleetCache()
		d.CredManager = &stubCredManager{}
	})
	if deps.Cache != nil {
		t.Fatal("precondition: L1 cache must be nil (disabled) for this decouple test")
	}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	req := freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"what is the capital of France?"}]}`)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Nexus-Cache"); got != "MISS" {
		t.Fatalf("X-Nexus-Cache=%q want MISS — with L1 off + L2 on the cache stage must stay active (decouple), not skip as disabled", got)
	}
	if !strings.Contains(w.Body.String(), "live-after-l2-miss") {
		t.Fatalf("want live upstream body after L2 miss; got:\n%s", w.Body.String())
	}
}
