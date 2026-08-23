package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// The cache stores CANONICAL bytes, so a HIT replays whatever the codec wrote
// into the gateway's namespace at write time — to a reader whose egress is the
// identity, that is a delivery. This is the second and last frame a non-stream
// response reaches the client through; the live one is egressReshapeNonStream.
//
// Both are needed and neither covers the other: a leak that never reached the
// live path can still be sitting in Redis from a build that predates the fix,
// and every HIT on that key serves it.
func TestCacheHitNonStream_DoesNotReplayTheNamespace(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"cached namespace"}]}`)
	deps := makeOpenAIDeps(t, "", emptyHookCache(t), cacheOpt)

	adapter, ok := deps.ProviderReg.Get(provcore.FormatOpenAI)
	if !ok {
		t.Fatal("openai adapter missing")
	}
	prepReq := provcore.Request{
		WireShape:  typology.WireShapeOpenAIChat,
		Body:       body,
		BodyFormat: provcore.FormatOpenAI,
	}
	prepReq.Target.ProviderModelID = "gpt-4o"
	finalBody, _, _, err := adapter.PrepareBody(prepReq)
	if err != nil {
		t.Fatalf("PrepareBody: %v", err)
	}

	// A canonical entry as a Responses-API target's decode would have produced
	// it: the answer, plus the carrier that egress encoder consumes. An
	// OpenAI-chat reader consumes neither.
	entry := &core.ResponseEntry{
		Provider: "openai",
		Model:    "gpt-4o",
		CanonicalResponse: []byte(`{"id":"chatcmpl-cached","object":"chat.completion","model":"gpt-4o",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"cached answer"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6},` +
			`"nexus":{"ext":{"openai":{"responses":{"id":"resp_cached","status":"completed"}}}}}`),
		Usage:           provcore.Usage{PromptTokens: iPtr(4), CompletionTokens: iPtr(2), TotalTokens: iPtr(6)},
		CachedAt:        time.Now().UTC(),
		OriginWireShape: typology.WireShapeOpenAIChat,
	}
	key := deps.Cache.BuildKey("openai", "gpt-4o", finalBody, "")
	if _, err := deps.Cache.StoreResponse(context.Background(), key, entry); err != nil {
		t.Fatalf("StoreResponse: %v", err)
	}

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.EqualFold(w.Header().Get("X-Nexus-Cache"), "hit") {
		t.Fatalf("x-nexus-cache=%q want HIT — this case must exercise the replay path, "+
			"and a MISS would make it assert nothing", w.Header().Get("X-Nexus-Cache"))
	}
	out := w.Body.String()
	if strings.Contains(out, `"nexus"`) {
		t.Errorf("the cache replayed the gateway's namespace to the client: %s", out)
	}
	if !strings.Contains(out, "cached answer") {
		t.Errorf("the cached answer was lost along with the namespace: %s", out)
	}
}
