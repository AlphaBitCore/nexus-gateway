package replicate

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Configure — no-op.

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

// ExtractRequest — additional branches: malformed body, tool_calls in messages,
// model field stamp.

func TestExtractRequest_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`not json`), "/v1/predictions")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestExtractRequest_ToolCallsInMessages(t *testing.T) {
	body := []byte(`{
		"model":"meta/llama-3-70b-instruct",
		"input":{"messages":[
			{"role":"assistant","content":"calling tool",
			 "tool_calls":[
				{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}
			 ]
			}
		]}
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/predictions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"f"`) {
		t.Errorf("ToolCallSegments=%v want one with name f", nc.ToolCallSegments)
	}
	if nc.Metadata["model"] != "meta/llama-3-70b-instruct" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

// ExtractResponse — additional branches: malformed body, missing id (ErrUnknownSchema),
// object-shape output with "text" key.

func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`not json`), "/v1/predictions/pred_1")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestExtractResponse_MissingID(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"status":"succeeded"}`), "/v1/predictions/pred_1")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractResponse_ObjectOutputText(t *testing.T) {
	body := []byte(`{"id":"pred_1","status":"succeeded","output":{"text":"hello","answer":"world"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/predictions/pred_1")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 {
		t.Errorf("Segments=%v want 2", nc.Segments)
	}
}

// ExtractStreamChunk — three cases: invalid JSON returns empty (no error),
// "output" string, "text" string.

func TestExtractStreamChunk_InvalidJSON_NoError(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte(`not json`), "/v1/predictions/pred_1")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
}

func TestExtractStreamChunk_OutputAndText(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(),
		[]byte(`{"output":"Hello ","text":"world"}`), "/v1/predictions/pred_1")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 || nc.Segments[0] != "Hello " || nc.Segments[1] != "world" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// DetectRequestMeta — every branch: nil request, Token (non-r8), Bearer,
// unrecognised header, body falls back to version when model absent.

func TestDetectRequestMeta_NilRequest(t *testing.T) {
	a := &Adapter{}
	got := a.DetectRequestMeta(nil, nil)
	if got.Provider != "replicate" {
		t.Errorf("Provider=%q", got.Provider)
	}
	if got.ApiKeyClass != "" {
		t.Errorf("nil request → empty class, got %q", got.ApiKeyClass)
	}
}

func TestDetectRequestMeta_PlainToken(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.replicate.com/v1/predictions", nil)
	r.Header.Set("Authorization", "Token plain-token-not-r8")
	meta := a.DetectRequestMeta(r, []byte(`{}`))
	if meta.ApiKeyClass != "replicate-token" {
		t.Errorf("ApiKeyClass=%q want replicate-token", meta.ApiKeyClass)
	}
	if meta.ApiKeyFingerprint == "" {
		t.Errorf("fingerprint empty")
	}
}

func TestDetectRequestMeta_BearerToken(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.replicate.com/v1/predictions", nil)
	r.Header.Set("Authorization", "Bearer some-bearer-key")
	meta := a.DetectRequestMeta(r, []byte(`{}`))
	if meta.ApiKeyClass != "replicate-bearer" {
		t.Errorf("ApiKeyClass=%q want replicate-bearer", meta.ApiKeyClass)
	}
	if meta.ApiKeyFingerprint == "" {
		t.Errorf("fingerprint empty")
	}
}

func TestDetectRequestMeta_EmptyTokenPayloads(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.replicate.com/v1/predictions", nil)
	r.Header.Set("Authorization", "Token   ") // whitespace-only
	meta := a.DetectRequestMeta(r, nil)
	if meta.ApiKeyClass != "" {
		t.Errorf("whitespace-only Token → empty class, got %q", meta.ApiKeyClass)
	}

	r2, _ := http.NewRequest(http.MethodPost, "https://api.replicate.com/v1/predictions", nil)
	r2.Header.Set("Authorization", "Bearer   ")
	meta2 := a.DetectRequestMeta(r2, nil)
	if meta2.ApiKeyClass != "" {
		t.Errorf("whitespace-only Bearer → empty class, got %q", meta2.ApiKeyClass)
	}
}

func TestDetectRequestMeta_UnknownScheme(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.replicate.com/v1/predictions", nil)
	r.Header.Set("Authorization", "Basic foo")
	meta := a.DetectRequestMeta(r, nil)
	if meta.ApiKeyClass != "" {
		t.Errorf("unknown scheme → empty class, got %q", meta.ApiKeyClass)
	}
}

func TestDetectRequestMeta_VersionFallback(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.replicate.com/v1/predictions", nil)
	// No model field, only version — model should fall back to version.
	body := []byte(`{"version":"abc123hash"}`)
	meta := a.DetectRequestMeta(r, body)
	if meta.Model != "abc123hash" {
		t.Errorf("Model=%q want version fallback abc123hash", meta.Model)
	}
}

func TestDetectRequestMeta_ModelPreferredOverVersion(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.replicate.com/v1/predictions", nil)
	body := []byte(`{"model":"meta/llama-3-70b-instruct","version":"abc123"}`)
	meta := a.DetectRequestMeta(r, body)
	if meta.Model != "meta/llama-3-70b-instruct" {
		t.Errorf("Model=%q want model preferred", meta.Model)
	}
}

// RewriteRequestBody / RewriteResponseBody — both unsupported.

func TestRewriteRequestBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"x":"y"}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/predictions", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
	if string(out) != string(body) {
		t.Errorf("body mutated: %s", out)
	}
}

func TestRewriteResponseBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"id":"pred_1","status":"succeeded"}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/predictions/pred_1", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
	if string(out) != string(body) {
		t.Errorf("body mutated: %s", out)
	}
}

// Normalize — Tier-1 dispatch via the unified extract helper. The adapter
// is decoded by the shared OpenAI Chat codec via the registry key.

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "replicate" {
		t.Errorf("ID=%q", a.ID())
	}
}

func TestExtractRequest_Prompt(t *testing.T) {
	body := []byte(`{
		"version":"meta/llama-3-70b-instruct:abc123",
		"input":{"prompt":"What is the capital of France?","system_prompt":"Be concise."}
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/predictions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["version"] != "meta/llama-3-70b-instruct:abc123" {
		t.Errorf("version=%q", nc.Metadata["version"])
	}
}

func TestExtractRequest_Messages(t *testing.T) {
	body := []byte(`{"input":{"messages":[{"role":"user","content":"hi"}]}}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/predictions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hi" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_NoInput(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"foo":"bar"}`), "/v1/predictions")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v", err)
	}
}

func TestExtractResponse_StringOutput(t *testing.T) {
	body := []byte(`{"id":"pred_1","status":"succeeded","output":"Paris."}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/predictions/pred_1")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Paris." {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["status"] != "succeeded" {
		t.Errorf("status=%q", nc.Metadata["status"])
	}
}

func TestExtractResponse_ArrayOutput(t *testing.T) {
	body := []byte(`{"id":"pred_1","status":"succeeded","output":["The ","capital ","is Paris."]}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/predictions/pred_1")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 3 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_ErrorField(t *testing.T) {
	body := []byte(`{"id":"pred_1","status":"failed","error":"prediction crashed"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/predictions/pred_1")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error meta=%q", nc.Metadata["error"])
	}
	found := false
	for _, s := range nc.Segments {
		if s == "prediction crashed" {
			found = true
		}
	}
	if !found {
		t.Errorf("error message missing from Segments: %v", nc.Segments)
	}
}

func TestDetectRequestMeta_R8Token(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.replicate.com/v1/predictions", nil)
	r.Header.Set("Authorization", "Token r8_abcdef")
	meta := a.DetectRequestMeta(r, []byte(`{}`))
	if meta.Provider != "replicate" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.ApiKeyClass != "replicate-token-r8" {
		t.Errorf("ApiKeyClass=%q", meta.ApiKeyClass)
	}
}

func TestDetectResponseUsage_NonLLM(t *testing.T) {
	a := &Adapter{}
	if a.DetectResponseUsage(nil, []byte(`{}`)).Status != traffic.UsageStatusNonLLM {
		t.Errorf("want non_llm")
	}
}
