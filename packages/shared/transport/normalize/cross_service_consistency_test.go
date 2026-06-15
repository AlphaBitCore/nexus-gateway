package normalize

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Cross-service consistency: same wire bytes captured by ai-gateway,
// compliance-proxy, and agent (via Hub) must produce byte-identical
// NormalizedPayload JSON. The three services wire shared/normalize
// through the same BuildAuditFn closure, so consistency is reduced to
// "the closure is deterministic with respect to its inputs". This test
// pins that invariant.
//
// We don't simulate the three services literally — each one ends up
// invoking BuildAuditFn(reg, metrics) with the same (adapterType,
// contentType, model, path, stream, body) tuple. If the closure is
// deterministic and the registry resolves identically, the output is
// identical. The test asserts both invariants.

func TestCrossServiceConsistency_OpenAIChat_Request(t *testing.T) {
	reg := NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	body := []byte(`{
	  "model": "gpt-4o-mini",
	  "messages": [
	    {"role": "system", "content": "You are helpful."},
	    {"role": "user", "content": "Hi"}
	  ],
	  "temperature": 0.5
	}`)

	fn := BuildAuditFn(reg, nil)
	if fn == nil {
		t.Fatal("BuildAuditFn returned nil")
	}

	// Simulate three callers with byte-identical inputs.
	raws := make([][]byte, 3)
	for i := range 3 {
		raw, status, _ := fn("request", "application/json", "openai", "gpt-4o-mini", "/v1/chat/completions", false, body)
		if status != "ok" {
			t.Fatalf("caller %d: status=%q expected ok", i, status)
		}
		raws[i] = raw
	}
	if string(raws[0]) != string(raws[1]) || string(raws[1]) != string(raws[2]) {
		t.Fatalf("cross-service output diverges:\nai-gateway: %s\ncp:         %s\nagent:      %s",
			raws[0], raws[1], raws[2])
	}
}

func TestCrossServiceConsistency_GeminiGenerate_Request(t *testing.T) {
	reg := NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	body := []byte(`{
	  "contents": [{"role": "user", "parts": [{"text": "Hello"}]}],
	  "generationConfig": {"temperature": 0.3}
	}`)

	fn := BuildAuditFn(reg, nil)
	if fn == nil {
		t.Fatal("BuildAuditFn returned nil")
	}

	raws := make([][]byte, 3)
	for i := range 3 {
		raw, status, _ := fn("request", "application/json", "gemini", "gemini-1.5-pro", "/v1beta/models/gemini-1.5-pro:generateContent", false, body)
		if status != "ok" {
			t.Fatalf("caller %d: status=%q expected ok", i, status)
		}
		raws[i] = raw
	}
	if string(raws[0]) != string(raws[1]) || string(raws[1]) != string(raws[2]) {
		t.Fatalf("cross-service Gemini output diverges:\nai-gateway: %s\ncp:         %s\nagent:      %s",
			raws[0], raws[1], raws[2])
	}
}

func TestCrossServiceConsistency_OAI_Compat_Provider_RoutesToOpenAI(t *testing.T) {
	// Operators frequently name a Gemini provider "google-gemini" (or
	// similar) and route its traffic through the OpenAI-compatible
	// /v1/chat/completions facade. The model row's adapter_type is
	// "openai" in that case — the registry must resolve to the OpenAI
	// normalizer regardless of the user-visible provider name, which is
	// the routing-key refactor's key invariant (P7.1a).
	reg := NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	body := []byte(`{
	  "model": "gemini-1.5-pro",
	  "messages": [{"role": "user", "content": "hi"}]
	}`)
	fn := BuildAuditFn(reg, nil)

	// adapterType="openai" (from the model row), even though the user-named
	// provider is "google-gemini".
	raw, status, _ := fn("request", "application/json", "openai", "gemini-1.5-pro", "/v1/chat/completions", false, body)
	if status != "ok" {
		t.Fatalf("status=%q expected ok", status)
	}
	var payload NormalizedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Protocol != "openai-chat" {
		t.Fatalf("expected openai-chat protocol when adapterType=openai, got %q", payload.Protocol)
	}
}

func TestCrossServiceConsistency_UnknownAdapter_FallsThroughToGeneric(t *testing.T) {
	// An adapter type the registry doesn't know about now falls through
	// the lookup chain and lands on the "*:*:*" generic-http normalizer.
	// Previously (before generic-http was registered) this produced
	// status="failed". The invariant we pin here: behaviour is
	// deterministic across all three producers AND the payload Kind
	// matches the body's content-type.
	reg := NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	fn := BuildAuditFn(reg, nil)
	for i := range 3 {
		raw, status, _ := fn("request", "application/json", "no-such-adapter", "x", "/v1/x", false, []byte(`{"x":1}`))
		if status != "ok" {
			t.Fatalf("iter %d: status=%q expected ok (generic fallback)", i, status)
		}
		if raw == nil {
			t.Fatalf("iter %d: raw should be populated, got nil", i)
		}
		// Generic-http payload — Kind=http-json, Protocol=generic-http.
		var payload NormalizedPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("iter %d: unmarshal: %v", i, err)
		}
		if payload.Kind != KindHTTPJSON {
			t.Fatalf("iter %d: Kind=%v want http-json", i, payload.Kind)
		}
		if payload.Protocol != "generic-http" {
			t.Fatalf("iter %d: Protocol=%q want generic-http", i, payload.Protocol)
		}
	}
}

func TestCrossServiceConsistency_DefaultAIBuiltins_ResolveCoverage(t *testing.T) {
	// Belt-and-braces: every adapter type registered by
	// RegisterDefaultAIBuiltins must resolve to a non-nil normalizer
	// when the registry is fed (adapterType, "", "") only — i.e. the
	// provider-only fallback (lookup step #3). This guards against a
	// future refactor accidentally registering only the path-specific
	// key without the provider-only fallback.
	reg := NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	cases := []string{
		"openai", "azure-openai", "deepseek", "glm", "groq", "perplexity",
		"mistral", "xai", "huggingface", "replicate", "together", "fireworks",
		"moonshot", "minimax", "cohere",
		"anthropic", "bedrock",
		"gemini", "vertex",
	}
	for _, adapter := range cases {
		got := reg.Resolve(Meta{AdapterType: adapter})
		if got == nil {
			t.Errorf("adapter %q: expected non-nil normalizer", adapter)
		}
	}
}

// Sanity: registry resolution must use AdapterType, not the legacy
// Provider field name (which no longer exists). This test would not
// compile if Meta still carried Provider — pin the field name shape
// here so a future refactor that re-adds Provider gets caught.
func TestCrossServiceConsistency_MetaUsesAdapterType(t *testing.T) {
	_ = Meta{AdapterType: "openai"} // compile-time check
	// If someone re-adds a Provider field, the struct literal above
	// stays valid (struct literals only mention selected fields), so
	// also runtime-check that the resolver routes on AdapterType:
	reg := NewRegistry()
	stub := &stubNormalizer{id: "x"}
	reg.Register("test-adapter", stub)
	reg.Freeze()

	if reg.Resolve(Meta{AdapterType: "test-adapter"}) != stub {
		t.Fatal("AdapterType routing broken")
	}
	if reg.Resolve(Meta{}) != nil {
		t.Fatal("Empty Meta should not match anything")
	}
}

// Smoke check: BuildAuditFn must accept the (direction, contentType,
// adapterType, model, path, stream, body) signature that all three
// services share. This is a compile-time pin against signature drift.
func TestCrossServiceConsistency_AuditFnSignature(t *testing.T) {
	reg := NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()
	fn := BuildAuditFn(reg, nil)

	// Call with named args via inline struct so a future re-order
	// catches the change at compile time.
	type call struct {
		direction   string
		contentType string
		adapterType string
		model       string
		path        string
		stream      bool
		body        []byte
	}
	c := call{
		direction:   "request",
		contentType: "application/json",
		adapterType: "openai",
		model:       "gpt-4o-mini",
		path:        "/v1/chat/completions",
		stream:      false,
		body:        []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`),
	}
	raw, status, _ := fn(c.direction, c.contentType, c.adapterType, c.model, c.path, c.stream, c.body)
	if status != "ok" || len(raw) == 0 {
		t.Fatalf("unexpected status=%q rawLen=%d", status, len(raw))
	}

	_ = context.Background() // keep import grounded
}

// Embedding normalizer three-source consistency tests
// Same invariant as the chat tests above: ai-gateway, compliance-proxy,
// and agent all call BuildAuditFn with the same (adapterType, contentType,
// model, path, stream, body) tuple. The output must be byte-identical
// across all three callers — the most important architectural invariant
// of the S3+S6 design.

func threeSourceConsistent(t *testing.T, fn core.AuditFn,
	direction, contentType, adapterType, model, path string, stream bool, body []byte) []byte {
	t.Helper()
	raws := make([][]byte, 3)
	for i := range 3 {
		raw, status, _ := fn(direction, contentType, adapterType, model, path, stream, body)
		if status != "ok" {
			t.Fatalf("caller %d: status=%q expected ok", i, status)
		}
		raws[i] = raw
	}
	if !bytes.Equal(raws[0], raws[1]) || !bytes.Equal(raws[1], raws[2]) {
		t.Fatalf("cross-service output diverges:\nai-gateway: %s\ncp:         %s\nagent:      %s",
			raws[0], raws[1], raws[2])
	}
	return raws[0]
}

func TestCrossServiceConsistency_OpenAIEmbeddings_Request(t *testing.T) {
	reg := NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	body := []byte(`{"model":"text-embedding-3-small","input":"hello world"}`)
	fn := BuildAuditFn(reg, nil)
	raw := threeSourceConsistent(t, fn,
		"request", "application/json", "openai", "text-embedding-3-small", "/v1/embeddings", false, body)

	var payload NormalizedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Kind != "ai-embedding" {
		t.Errorf("kind = %v, want ai-embedding", payload.Kind)
	}
	if payload.Model != "text-embedding-3-small" {
		t.Errorf("model = %q", payload.Model)
	}
	if len(payload.Inputs) != 1 || payload.Inputs[0] != "hello world" {
		t.Errorf("inputs = %v", payload.Inputs)
	}
}

func TestCrossServiceConsistency_OpenAIEmbeddings_Response(t *testing.T) {
	reg := NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	body := []byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"text-embedding-3-small","usage":{"prompt_tokens":3,"total_tokens":3}}`)
	fn := BuildAuditFn(reg, nil)
	raw := threeSourceConsistent(t, fn,
		"response", "application/json", "openai", "text-embedding-3-small", "/v1/embeddings", false, body)

	var payload NormalizedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Kind != "ai-embedding" {
		t.Errorf("kind = %v, want ai-embedding", payload.Kind)
	}
	if payload.Inputs != nil {
		t.Errorf("inputs should be nil on response side, got %v", payload.Inputs)
	}
}

func TestCrossServiceConsistency_CohereEmbeddings_Request(t *testing.T) {
	reg := NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	body := []byte(`{"texts":["hello","world"],"model":"embed-v4.0","input_type":"search_document"}`)
	fn := BuildAuditFn(reg, nil)
	raw := threeSourceConsistent(t, fn,
		"request", "application/json", "cohere", "embed-v4.0", "/v1/embed", false, body)

	var payload NormalizedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Kind != "ai-embedding" {
		t.Errorf("kind = %v, want ai-embedding", payload.Kind)
	}
	if len(payload.Inputs) != 2 {
		t.Errorf("inputs = %v, want 2 items", payload.Inputs)
	}
}

func TestCrossServiceConsistency_CohereEmbeddings_Response(t *testing.T) {
	reg := NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	body := []byte(`{"id":"emb-test","model":"embed-v4.0","embeddings":{"float":[[0.1,0.2]]},"meta":{"billed_units":{"input_tokens":7}},"response_type":"embeddings_floats"}`)
	fn := BuildAuditFn(reg, nil)
	raw := threeSourceConsistent(t, fn,
		"response", "application/json", "cohere", "embed-v4.0", "/v1/embed", false, body)

	var payload NormalizedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Kind != "ai-embedding" {
		t.Errorf("kind = %v, want ai-embedding", payload.Kind)
	}
	if payload.Inputs != nil {
		t.Errorf("inputs should be nil on response side, got %v", payload.Inputs)
	}
}

func TestCrossServiceConsistency_GeminiEmbeddings_Single_Request(t *testing.T) {
	reg := NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	body := []byte(`{"model":"models/text-embedding-004","content":{"parts":[{"text":"hello world"}]}}`)
	fn := BuildAuditFn(reg, nil)
	raw := threeSourceConsistent(t, fn,
		"request", "application/json", "gemini", "text-embedding-004",
		"/v1beta/models/text-embedding-004:embedContent", false, body)

	var payload NormalizedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Kind != "ai-embedding" {
		t.Errorf("kind = %v, want ai-embedding", payload.Kind)
	}
	if len(payload.Inputs) != 1 || payload.Inputs[0] != "hello world" {
		t.Errorf("inputs = %v", payload.Inputs)
	}
}

func TestCrossServiceConsistency_GeminiEmbeddings_Single_Response(t *testing.T) {
	reg := NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	body := []byte(`{"embedding":{"values":[0.1,0.2,0.3]}}`)
	fn := BuildAuditFn(reg, nil)
	raw := threeSourceConsistent(t, fn,
		"response", "application/json", "gemini", "text-embedding-004",
		"/v1beta/models/text-embedding-004:embedContent", false, body)

	var payload NormalizedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Kind != "ai-embedding" {
		t.Errorf("kind = %v, want ai-embedding", payload.Kind)
	}
	if payload.Inputs != nil {
		t.Errorf("inputs should be nil on response side, got %v", payload.Inputs)
	}
}

func TestCrossServiceConsistency_GeminiEmbeddings_Batch_Request(t *testing.T) {
	reg := NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	body := []byte(`{"requests":[{"model":"models/text-embedding-004","content":{"parts":[{"text":"foo"}]}},{"model":"models/text-embedding-004","content":{"parts":[{"text":"bar"}]}}]}`)
	fn := BuildAuditFn(reg, nil)
	raw := threeSourceConsistent(t, fn,
		"request", "application/json", "gemini", "text-embedding-004",
		"/v1beta/models/text-embedding-004:batchEmbedContents", false, body)

	var payload NormalizedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Kind != "ai-embedding" {
		t.Errorf("kind = %v, want ai-embedding", payload.Kind)
	}
	if len(payload.Inputs) != 2 {
		t.Errorf("inputs = %v, want 2 items", payload.Inputs)
	}
}

func TestCrossServiceConsistency_GeminiEmbeddings_Batch_Response(t *testing.T) {
	reg := NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	body := []byte(`{"embeddings":[{"values":[0.1,0.2]},{"values":[0.3,0.4]}]}`)
	fn := BuildAuditFn(reg, nil)
	raw := threeSourceConsistent(t, fn,
		"response", "application/json", "gemini", "text-embedding-004",
		"/v1beta/models/text-embedding-004:batchEmbedContents", false, body)

	var payload NormalizedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Kind != "ai-embedding" {
		t.Errorf("kind = %v, want ai-embedding", payload.Kind)
	}
	if payload.Inputs != nil {
		t.Errorf("inputs should be nil on response side, got %v", payload.Inputs)
	}
}

// TestGeminiIngress_NonStreamResponse_NonGeminiUpstream_NormalizesToAIChat
// reproduces the prod bug where a non-stream request to a NON-Gemini model
// (e.g. OpenAI o1) served over the Gemini `:generateContent` ingress had
// its RESPONSE captured in Gemini `candidates[]` wire shape (the gateway
// re-encodes the upstream OpenAI reply back to the client's Gemini ingress
// shape before capture), yet the audit layer keyed the response normalizer
// on the routed upstream adapter ("openai"). The OpenAI normalizer rejected
// the `candidates[]` body and the row fell through to Tier-3 http-json.
//
// The fix keys shared/normalize on the INGRESS format for both directions
// (the only wire shape ai-gateway ever captures), so the audit closure is
// invoked with adapterType="gemini" here. The Gemini normalizer claims the
// body at Tier-1 → kind=ai-chat with messages[0].content as a JSON array
// carrying the visible assistant text.
//
// Uses the full production registry (Tier 1+2+3) to prove this is a clean
// Tier-1 claim, not a Tier-2/3 rescue.
func TestGeminiIngress_NonStreamResponse_NonGeminiUpstream_NormalizesToAIChat(t *testing.T) {
	reg := BuildRegistry()
	fn := BuildAuditFn(reg, nil)
	if fn == nil {
		t.Fatal("BuildAuditFn returned nil")
	}

	// Verbatim shape of the prod-captured response for model `o1` requested
	// via the Gemini ingress: OpenAI upstream reply RE-ENCODED to Gemini wire
	// (candidates[].content.parts[].text + usageMetadata).
	geminiShapedResponse := []byte(`{
	  "candidates": [{
	    "content": {"role": "model", "parts": [{"text": "Paris is the capital of France."}]},
	    "finishReason": "STOP"
	  }],
	  "responseId": "chatcmpl-abc123",
	  "modelVersion": "o1-2024-12-17",
	  "usageMetadata": {"promptTokenCount": 12, "candidatesTokenCount": 8, "thoughtsTokenCount": 256, "totalTokenCount": 276}
	}`)

	// adapterType="gemini" is what normalizeAdapterType now yields for a
	// Gemini-ingress row (rec.IngressFormat="gemini"), regardless of the
	// routed upstream being OpenAI.
	raw, status, errReason := fn("response", "application/json", "gemini", "o1",
		"/v1beta/models/o1:generateContent", false, geminiShapedResponse)
	if status != "ok" {
		t.Fatalf("status=%q errReason=%q, want ok", status, errReason)
	}

	var payload NormalizedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Primary assertion: kind=ai-chat (NOT http-json).
	if payload.Kind != KindAIChat {
		t.Fatalf("Kind=%v, want %v (regression: fell through to Tier-3)", payload.Kind, KindAIChat)
	}
	// Protocol must be the Gemini generate normalizer, proving a Tier-1
	// claim rather than a generic-http catch-all.
	if payload.Protocol != "gemini-generate" {
		t.Fatalf("Protocol=%q, want gemini-generate", payload.Protocol)
	}

	// Business assertion: messages[last].content is a non-empty ARRAY whose
	// text block carries the visible assistant answer.
	if len(payload.Messages) == 0 {
		t.Fatalf("Messages empty, want at least one assistant message")
	}
	last := payload.Messages[len(payload.Messages)-1]
	if len(last.Content) == 0 {
		t.Fatalf("messages[last].content is empty, want a content array with the visible text")
	}
	var visible string
	for _, block := range last.Content {
		if block.Text != "" {
			visible = block.Text
			break
		}
	}
	if visible != "Paris is the capital of France." {
		t.Fatalf("visible text = %q, want %q", visible, "Paris is the capital of France.")
	}

	// Verify the serialised wire form really has content as a JSON array
	// (the Control Plane reader + Agent UI depend on this), not a scalar.
	var wire struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("wire unmarshal: %v", err)
	}
	if len(wire.Messages) == 0 {
		t.Fatalf("wire messages empty")
	}
	wireContent := bytes.TrimSpace(wire.Messages[len(wire.Messages)-1].Content)
	if len(wireContent) == 0 || wireContent[0] != '[' {
		t.Fatalf("messages[last].content wire form = %s, want a JSON array", wireContent)
	}

	// Token usage must survive the Gemini normalizer so cost/analytics stay
	// correct (candidatesTokenCount = visible output tokens).
	if payload.Usage == nil {
		t.Fatalf("Usage nil, want promptTokenCount/candidatesTokenCount extracted")
	}
}
