// stage_respond_test.go — characterization pins for the execute/respond
// tail of the proxy pipeline: the X-Nexus-Coerced header on the direct
// streaming path, and the L2 semantic write-back scheduled from the
// direct non-streaming MISS path using the canonical messages.
package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	sharednormalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// TestServeProxy_Stream_DirectPath_EmitsCoercedHeader pins the rewrite
// disclosure on the direct streaming path: when the adapter rewrites the
// request for the routed model (reasoning models rename max_tokens), the
// response carries X-Nexus-Coerced listing the applied rewrites.
func TestServeProxy_Stream_DirectPath_EmitsCoercedHeader(t *testing.T) {
	upstream := openAIStreamUpstream(t, []string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"o3-mini","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"o3-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	})
	defer upstream.Close()

	deps := makeOpenAIDeps(t, upstream.URL, emptyHookCache(t), func(d *Deps) {
		// Route to a reasoning model so the adapter applies the
		// max_tokens→max_completion_tokens passthrough rewrite and the
		// executor reports it via Coerced.
		d.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
			ProviderID:      "p-openai",
			ProviderName:    "openai",
			ProviderModelID: "o3-mini",
			ModelID:         "o3-mini",
			ModelName:       "o3 mini",
			ModelCode:       "o3-mini",
			AdapterType:     "openai",
		}}}
	})

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
		Stream:     true,
	})
	body := `{"model":"gpt-4o","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	coerced := w.Header().Get("X-Nexus-Coerced")
	if !strings.Contains(coerced, "max_tokens") {
		t.Errorf("X-Nexus-Coerced=%q want the max_tokens rewrite disclosed", coerced)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Errorf("stream body=%s want upstream deltas relayed", w.Body.String())
	}
}

// TestServeProxy_DirectNonStreamMISS_SchedulesL2SemanticWriteBack pins
// the L2 write-back on the direct (brokerless) non-streaming MISS path:
// the canonical messages produced at request build feed the semantic
// writer, whose WriteRequest carries the caller's prompt text as the
// embedding input and response kind "response".
func TestServeProxy_DirectNonStreamMISS_SchedulesL2SemanticWriteBack(t *testing.T) {
	upstream := openAIChatUpstream(t, `{
		"id":"x","object":"chat.completion","model":"gpt-4o",
		"choices":[{"index":0,"message":{"role":"assistant","content":"fresh"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
	}`)
	defer upstream.Close()

	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	writer := newStubWriter()
	deps := makeOpenAIDeps(t, upstream.URL, emptyHookCache(t), cacheOpt, func(d *Deps) {
		d.NormalizeRegistry = sharednormalize.BuildRegistry()
		d.SemanticWriter = writer
		d.SemanticConfigCache = enabledFleetCache()
		d.CredManager = &stubCredManager{}
	})

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	w := httptest.NewRecorder()
	h(w, freshChatRequest(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"semantic write probe"}]}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Nexus-Cache"); got != "MISS" {
		t.Errorf("X-Nexus-Cache=%q want MISS", got)
	}
	select {
	case <-writer.writeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("semantic writer did not fire within 3s on the direct MISS path")
	}
	req := writer.request()
	if !strings.Contains(req.EmbeddingInput, "semantic write probe") {
		t.Errorf("EmbeddingInput=%q want the canonical prompt text", req.EmbeddingInput)
	}
	if req.ResponseKind != "response" {
		t.Errorf("ResponseKind=%q want response (non-streaming)", req.ResponseKind)
	}
}
