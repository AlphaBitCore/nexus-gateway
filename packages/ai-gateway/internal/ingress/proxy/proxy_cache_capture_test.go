package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	goHooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// stubVKAuthCacheTest returns a fixed VKMeta — no DB lookup. The cache
// HIT branch needs a non-nil VKMeta so the request reaches the cache
// phase; nothing else about it is exercised by this test.
type stubVKAuthCacheTest struct{ meta *vkauth.VKMeta }

func (s *stubVKAuthCacheTest) Authenticate(_ context.Context, _ *http.Request) (*vkauth.VKMeta, error) {
	return s.meta, nil
}

// stubRouterCacheTest returns a fixed routing target so route resolution
// completes without a real Resolver. AdapterType "openai" matches the
// OpenAI ingress under filterCompatibleTargets's legacy mode.
type stubRouterCacheTest struct{ targets []routingcore.RoutingTarget }

func (s *stubRouterCacheTest) ResolveTargets(_ context.Context, _ *routingcore.RoutingContext) (*routingcore.RouteResult, error) {
	return &routingcore.RouteResult{Targets: s.targets, RuleID: "rule-test", RuleName: "Test rule"}, nil
}

// captureProducer collects MQ enqueues so the test can inspect the audit
// envelope produced by audit.Writer. Only Enqueue is exercised.
type captureProducer struct {
	mu       sync.Mutex
	messages [][]byte
}

func (c *captureProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (c *captureProducer) Enqueue(_ context.Context, _ string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, append([]byte(nil), data...))
	return nil
}
func (c *captureProducer) Close() error { return nil }

// TestServeProxy_CacheHIT_RespectsResponseBodyCapture pins the regression
// fixed in this commit: the cache HIT early return at proxy.go:353 was
// returning before the response-body capture site at handleNonStream
// (proxy.go:1452), so admins enabling StoreResponseBody saw the bytes
// only on cache MISS. Both rows must now carry the response body.
//
// The test drives ServeProxy through a populated cache and asserts the
// emitted audit MQ message reflects the runtime payload-capture flag.
func TestServeProxy_CacheHIT_RespectsResponseBodyCapture(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	cachedResp := []byte(`{"id":"cached-1","choices":[{"message":{"role":"assistant","content":"cached-hello"}}]}`)

	tests := []struct {
		name             string
		captureRequest   bool
		captureResponse  bool
		wantRequestBody  []byte
		wantResponseBody []byte
	}{
		{
			name:             "both flags on captures both bodies",
			captureRequest:   true,
			captureResponse:  true,
			wantRequestBody:  body,
			wantResponseBody: cachedResp,
		},
		{
			name:             "response off skips response only",
			captureRequest:   true,
			captureResponse:  false,
			wantRequestBody:  body,
			wantResponseBody: nil,
		},
		{
			name:             "request off, response on captures response only",
			captureRequest:   false,
			captureResponse:  true,
			wantRequestBody:  nil,
			wantResponseBody: cachedResp,
		},
		{
			name:             "both off captures nothing",
			captureRequest:   false,
			captureResponse:  false,
			wantRequestBody:  nil,
			wantResponseBody: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mini, err := miniredis.Run()
			if err != nil {
				t.Fatalf("miniredis: %v", err)
			}
			defer mini.Close()
			rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})

			rcache := cache.New(rdb, cache.Config{Enabled: true}, slog.Default())
			if rcache == nil {
				t.Fatal("cache.New returned nil — config not honoured")
			}

			// Cache key: PrepareBody output (post-passthrough rewrite +
			// canonical JSON sort). For an OpenAI ingress hitting an
			// OpenAI-format target with matching ProviderModelID, PrepareBody
			// re-marshals the body with sorted keys; the cache key formula
			// re-canonicalizes so the resulting key is stable.
			cacheKey := rcache.BuildKey("openai", "gpt-4o", body, "")
			tIn, tOut, tTotal := 3, 4, 7
			usage := provcore.Usage{PromptTokens: &tIn, CompletionTokens: &tOut, TotalTokens: &tTotal}
			if _, err := rcache.StoreResponse(context.Background(), cacheKey, &cache.ResponseEntry{
				Provider:          "openai",
				Model:             "gpt-4o",
				CanonicalResponse: cachedResp,
				Usage:             usage,
				CachedAt:          time.Now().UTC(),
			}); err != nil {
				t.Fatalf("seed cache: %v", err)
			}

			hookCache := compliance.NewHookConfigCache(
				func(_ context.Context) ([]goHooks.HookConfig, error) { return nil, nil },
				builtins.Registry, 0, slog.Default(),
			)
			if err := hookCache.Start(context.Background()); err != nil {
				t.Fatalf("hookCache.Start: %v", err)
			}
			time.Sleep(20 * time.Millisecond) // first load

			prod := &captureProducer{}
			auditWriter := audit.NewWriter(prod, "nexus.event.ai-traffic", nil, slog.Default())

			pcStore := payloadcapture.NewStore(payloadcapture.Config{
				StoreRequestBody:   tc.captureRequest,
				StoreResponseBody:  tc.captureResponse,
				MaxInlineBodyBytes: 64 * 1024,
			})

			provReg := provcore.NewRegistry()
			provbuiltins.Register(provReg, nil, slog.Default())
			provReg.Freeze()

			deps := &Deps{
				VKAuth: &stubVKAuthCacheTest{meta: &vkauth.VKMeta{
					ID:               "vk-1",
					Name:             "test-vk",
					OrganizationID:   "org-1",
					OrganizationName: "Org",
				}},
				Router: &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
					ProviderID:      "p-openai",
					ProviderName:    "openai",
					ProviderModelID: "gpt-4o",
					ModelID:         "gpt-4o",
					ModelName:       "GPT-4o",
					AdapterType:     "openai",
				}}},
				HookConfigCache: hookCache,
				ProviderReg:     provReg,
				Cache:           rcache,
				AuditWriter:     auditWriter,
				PayloadCapture:  pcStore,
				Logger:          slog.Default(),
			}

			h := NewHandler(deps).ServeProxy(Ingress{
				WireShape:  typology.WireShapeOpenAIChat,
				BodyFormat: provcore.FormatOpenAI,
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer fake-token")
			w := httptest.NewRecorder()
			h(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			if got := w.Header().Get("X-Nexus-Cache"); got != "HIT" {
				t.Fatalf("x-nexus-cache = %q, want HIT (request did not hit cache)", got)
			}

			// Close drains the writer's buffer through the producer so
			// the captured slice is complete by the time we read it.
			auditWriter.Close()

			prod.mu.Lock()
			msgs := append([][]byte(nil), prod.messages...)
			prod.mu.Unlock()
			if len(msgs) != 1 {
				t.Fatalf("captured %d audit messages, want 1", len(msgs))
			}

			var evt mq.TrafficEventMessage
			if err := json.Unmarshal(msgs[0], &evt); err != nil {
				t.Fatalf("unmarshal audit envelope: %v", err)
			}
			if evt.CacheStatus != string(audit.CacheStatusHit) {
				t.Errorf("CacheStatus = %q, want %q", evt.CacheStatus, string(audit.CacheStatusHit))
			}
			if !rawJSONEqual(evt.RequestBody.InlineBytes, tc.wantRequestBody) {
				t.Errorf("RequestBody = %q, want %q", string(evt.RequestBody.InlineBytes), string(tc.wantRequestBody))
			}
			if !rawJSONEqual(evt.ResponseBody.InlineBytes, tc.wantResponseBody) {
				t.Errorf("ResponseBody = %q, want %q", string(evt.ResponseBody.InlineBytes), string(tc.wantResponseBody))
			}
		})
	}
}

// rawJSONEqual compares two json.RawMessage / []byte values, treating
// nil and zero-length identically — both represent "field omitted from
// the audit envelope" downstream.
func rawJSONEqual(a json.RawMessage, want []byte) bool {
	if len(a) == 0 && len(want) == 0 {
		return true
	}
	return string(a) == string(want)
}
