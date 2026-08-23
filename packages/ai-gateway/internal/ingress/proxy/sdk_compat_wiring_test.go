package proxy

// sdk_compat_wiring_test.go — serve-level proof that the AP-3 response-side
// guarantees are WIRED IN, not merely implemented.
//
// embedding_encoding_test.go and json_object_unwrap_test.go test the helpers as
// pure functions. That is necessary and not sufficient: a helper can be perfect
// and still never be called, or be called before the reshape that undoes it. The
// tests here drive a real request through ServeProxy — admission, the request-side
// metadata stamps, routing, the cache-HIT reader, the egress reshape — and assert
// on the bytes the CLIENT receives.
//
// They use a seeded cache entry (or a stub executor) as the response source
// rather than a live upstream, so they need no provider credentials and no
// network: the cached body plays the part of the upstream reply and the response
// pipeline under test is the same one. That keeps them runnable on a machine whose
// provider credentials are absent, expired, or encrypted under a key the current
// deployment no longer holds — none of which should stop a wiring regression from
// being caught.

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/tidwall/gjson"

	cache "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// TestServe_EmbeddingsBase64_IsHonouredEndToEnd is the regression test for the
// AP-3 corruption bug: the OpenAI SDKs request encoding_format="base64"
// implicitly and then decode the reply as packed float32, so a float array
// arriving in its place is read as a quarter-length garbage vector.
//
// Asserts on the served bytes that the vector comes back as base64 whose decoded
// width matches the original component count.
func TestServe_EmbeddingsBase64_IsHonouredEndToEnd(t *testing.T) {
	const model = "text-embedding-3-small"
	reqBody := []byte(`{"model":"` + model + `","input":"probe","encoding_format":"base64"}`)

	// The "upstream" reply: canonical OpenAI embeddings, float array. Embeddings
	// are not served from the response cache, so the stub upstream (fakeExecutor)
	// is what stands in for the provider here.
	want := []float32{1.5, -2.25, 0.75, 4}
	w := serveOnceWithUpstream(t, Ingress{
		WireShape:  typology.WireShapeOpenAIEmbeddings,
		BodyFormat: provcore.FormatOpenAI,
	}, "/v1/embeddings", reqBody, embeddingsFloatBody(want))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	emb := gjson.Get(w.Body.String(), "data.0.embedding")
	if emb.Type != gjson.String {
		t.Fatalf("encoding_format=base64 must yield a base64 STRING, got %s: %s", emb.Type, w.Body.String())
	}
	raw, err := base64.StdEncoding.DecodeString(emb.Str)
	if err != nil {
		t.Fatalf("served payload is not valid base64: %v", err)
	}
	if len(raw) != len(want)*4 {
		t.Fatalf("payload is %d bytes for %d components, want %d — an SDK would report %d components",
			len(raw), len(want), len(want)*4, len(raw)/4)
	}
	for i, expect := range want {
		got := math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
		if got != expect {
			t.Errorf("component %d = %v, want %v", i, got, expect)
		}
	}
}

// TestServe_EmbeddingsFloat_StaysFloatEndToEnd is the other half: an explicit
// float request must NOT be re-encoded, or a caller expecting number[] gets a
// string.
func TestServe_EmbeddingsFloat_StaysFloatEndToEnd(t *testing.T) {
	const model = "text-embedding-3-small"
	reqBody := []byte(`{"model":"` + model + `","input":"probe","encoding_format":"float"}`)

	w := serveOnceWithUpstream(t, Ingress{
		WireShape:  typology.WireShapeOpenAIEmbeddings,
		BodyFormat: provcore.FormatOpenAI,
	}, "/v1/embeddings", reqBody, embeddingsFloatBody([]float32{1, 2, 3}))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if emb := gjson.Get(w.Body.String(), "data.0.embedding"); !emb.IsArray() {
		t.Fatalf("encoding_format=float must stay a JSON array, got %s: %s", emb.Type, w.Body.String())
	}
}

// TestServe_JSONObjectFence_IsStrippedEndToEnd pins the json_object parseability
// guarantee through the full serve path. A model with no native JSON mode is
// steered by a system instruction it can ignore — claude-haiku-4-5 answered with
// a ```json fence on staging, which json.loads/JSON.parse reject outright.
func TestServe_JSONObjectFence_IsStrippedEndToEnd(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	deps := makeOpenAIDeps(t, "", emptyHookCache(t), cacheOpt)

	reqBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"give me json"}],` +
		`"response_format":{"type":"json_object"}}`)

	fenced := "```json\n{\n  \"name\": \"Margaret Chen\",\n  \"age\": 34\n}\n```"
	entry := &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(chatCompletionBody(fenced)),
		Usage:             provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2)},
		CachedAt:          time.Now().UTC(),
		OriginWireShape:   typology.WireShapeOpenAIChat,
	}
	seedResponseEntry(t, deps, "gpt-4o", reqBody, entry)

	w := serveOnce(t, deps, Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	}, "/v1/chat/completions", reqBody)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	content := gjson.Get(w.Body.String(), "choices.0.message.content").Str
	if strings.Contains(content, "```") {
		t.Fatalf("json_object content still carries a markdown fence: %q", content)
	}
	if !gjson.Valid(content) {
		t.Fatalf("json_object content must parse as JSON; got %q", content)
	}
	if name := gjson.Get(content, "name").Str; name != "Margaret Chen" {
		t.Errorf("unwrap lost payload data: name=%q in %q", name, content)
	}
}

// TestServe_NoJSONObjectRequest_LeavesFenceAlone — without json_object the fence
// is the model's own formatting choice and must survive.
func TestServe_NoJSONObjectRequest_LeavesFenceAlone(t *testing.T) {
	cacheOpt, cleanup := withCache(t)
	defer cleanup()
	deps := makeOpenAIDeps(t, "", emptyHookCache(t), cacheOpt)

	reqBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"show me json"}]}`)

	fenced := "```json\n{\"a\":1}\n```"
	entry := &cache.ResponseEntry{
		Provider:          "openai",
		Model:             "gpt-4o",
		CanonicalResponse: json.RawMessage(chatCompletionBody(fenced)),
		Usage:             provcore.Usage{PromptTokens: iPtr(1), CompletionTokens: iPtr(1), TotalTokens: iPtr(2)},
		CachedAt:          time.Now().UTC(),
		OriginWireShape:   typology.WireShapeOpenAIChat,
	}
	seedResponseEntry(t, deps, "gpt-4o", reqBody, entry)

	w := serveOnce(t, deps, Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	}, "/v1/chat/completions", reqBody)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if content := gjson.Get(w.Body.String(), "choices.0.message.content").Str; !strings.Contains(content, "```") {
		t.Errorf("fence was stripped without a json_object request: %q", content)
	}
}

// TestServe_UnmountedV1Path_ReturnsJSONEnvelope covers the F5 fallback at the
// mux level: Go's ServeMux default is `404 page not found` as text/plain, which
// both OpenAI SDKs surface as an error carrying no message.
func TestServe_UnmountedV1Path_ReturnsJSONEnvelope(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Mirrors the fallback registered in cmd/ai-gateway/wiring/routes.go.
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		envelope.WriteEndpointNotSupported(w, r.URL.Path)
	})

	for _, path := range []string{"/v1/completions", "/v1/moderations", "/v1/images/edits"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))

			if w.Code != http.StatusNotFound {
				t.Fatalf("status=%d want 404", w.Code)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type=%q want application/json", ct)
			}
			if msg := gjson.Get(w.Body.String(), "error.message").Str; !strings.Contains(msg, path) {
				t.Errorf("message %q should name %q", msg, path)
			}
		})
	}

	// A registered route must still win over the fallback.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if w.Code != http.StatusOK {
		t.Errorf("fallback shadowed a registered route: status=%d", w.Code)
	}
}

// --- helpers ---

// serveOnceWithUpstream drives ServeProxy against a stub upstream that returns
// upstreamBody verbatim. Used where the response cache cannot stand in for the
// provider (embeddings are not cached).
func serveOnceWithUpstream(t *testing.T, in Ingress, path string, body, upstreamBody []byte) *httptest.ResponseRecorder {
	t.Helper()
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       upstreamBody,
		Usage:      provcore.Usage{PromptTokens: iPtr(1), TotalTokens: iPtr(1)},
	}}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	return serveOnce(t, deps, in, path, body)
}

func serveOnce(t *testing.T, deps *Deps, in Ingress, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	h := NewHandler(deps).ServeProxy(in)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

func embeddingsFloatBody(vec []float32) []byte {
	type row struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	out, _ := json.Marshal(map[string]any{
		"object": "list",
		"model":  "text-embedding-3-small",
		"data":   []row{{Object: "embedding", Index: 0, Embedding: vec}},
		"usage":  map[string]any{"prompt_tokens": 1, "total_tokens": 1},
	})
	return out
}

func chatCompletionBody(content string) []byte {
	out, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl_wiring",
		"object":  "chat.completion",
		"created": 1747353700,
		"model":   "gpt-4o",
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
	return out
}
