package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// The rerank canonical carries a documents ceiling as a BILLING guard: rerank
// bills per search unit (one per 100 documents), so an unbounded array
// multiplies upstream spend from a single request. That guard lived inside the
// canonicalization branch, which the stage enters only when the ingress body
// format differs from the target's — and /v1/rerank's ingress format IS
// cohere, so on a native Cohere target, the common path, the whole validation
// block was skipped and the array went upstream unbounded.
//
// The published API reference states the 1..1000 contract, so this is also the
// code failing to enforce what the docs promise.
func serveRerankToCohereTarget(t *testing.T, upstreamURL string, body string) (*httptest.ResponseRecorder, func() int32) {
	t.Helper()
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(upstream.Close)
	if upstreamURL == "" {
		upstreamURL = upstream.URL
	}

	deps := makeOpenAIDeps(t, upstreamURL, emptyHookCache(t), func(d *Deps) {
		d.Router = &stubRouterCacheTest{targets: []routingcore.RoutingTarget{{
			ProviderID:      "p-cohere",
			ProviderName:    "cohere",
			ProviderModelID: "rerank-v3.5",
			ModelID:         "rerank-v3.5",
			ModelName:       "Rerank v3.5",
			ModelCode:       "rerank-v3.5",
			AdapterType:     "cohere",
		}}}
	})
	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeCohereRerank,
		BodyFormat: provcore.FormatCohere,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, r)
	return w, upstreamHits.Load
}

func rerankBody(t *testing.T, docs int) string {
	t.Helper()
	d := make([]string, docs)
	for i := range d {
		d[i] = "doc"
	}
	b, err := json.Marshal(map[string]any{
		"model": "rerank-v3.5", "query": "q", "documents": d,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestServeProxy_RerankNativeCohere_DocumentCeilingBinds(t *testing.T) {
	w, hits := serveRerankToCohereTarget(t, "", rerankBody(t, 1001))

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — the 1000-document billing guard must bind on the native Cohere leg, not only when the target format differs; body=%s",
			w.Code, w.Body.String())
	}
	if got := hits(); got != 0 {
		t.Errorf("upstream called %d times; an over-ceiling request must be refused before it can multiply rerank spend", got)
	}
	if !strings.Contains(w.Body.String(), "documents") {
		t.Errorf("error body should name the offending field; got %s", w.Body.String())
	}
}

func TestServeProxy_RerankNativeCohere_WithinCeilingStillServes(t *testing.T) {
	w, hits := serveRerankToCohereTarget(t, "", rerankBody(t, 2))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a valid rerank must still reach the provider; body=%s", w.Code, w.Body.String())
	}
	if got := hits(); got != 1 {
		t.Errorf("upstream called %d times, want 1", got)
	}
}

// Cohere serves object-shaped documents — measured against a live deployment,
// {"text": "..."} entries return 200 with correct relevance scores. The
// canonical validator rejects them because the canonical→Voyage codec has to
// read document text, which is a translation requirement, not the provider's.
// Enforcing it on the passthrough leg would 400 a body the provider would
// have served: a regression wearing a guard's clothes. The ceiling still
// binds, because that one protects us.
func TestServeProxy_RerankNativeCohere_ObjectDocumentsStillServe(t *testing.T) {
	body := `{"model":"rerank-v3.5","query":"q","documents":[{"text":"Paris is the capital of France."},{"text":"Bananas are yellow."}],"top_n":2}`
	w, hits := serveRerankToCohereTarget(t, "", body)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — object documents are the provider's own shape; body=%s", w.Code, w.Body.String())
	}
	if got := hits(); got != 1 {
		t.Errorf("upstream called %d times, want 1", got)
	}
}

func TestServeProxy_RerankNativeCohere_ObjectDocumentsOverCeilingRefused(t *testing.T) {
	docs := make([]string, 1001)
	for i := range docs {
		docs[i] = `{"text":"d"}`
	}
	body := `{"model":"rerank-v3.5","query":"q","documents":[` + strings.Join(docs, ",") + `]}`
	w, hits := serveRerankToCohereTarget(t, "", body)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — the billing ceiling counts entries whatever their shape; body=%s", w.Code, w.Body.String())
	}
	if got := hits(); got != 0 {
		t.Errorf("upstream called %d times, want 0", got)
	}
}
