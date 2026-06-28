package huggingface

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "huggingface" {
		t.Errorf("ID=%q", a.ID())
	}
}

// TGI / OpenAI-compat path

func TestExtractRequest_TGIChatCompletions(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"hi from HF TGI"}],
		"model":"meta-llama/Llama-3.3-70B-Instruct"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hi from HF TGI" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_TGIChatCompletions(t *testing.T) {
	body := []byte(`{
		"choices":[{"message":{"role":"assistant","content":"hello back"}, "finish_reason":"stop"}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hello back" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// Legacy serverless inference path

func TestExtractRequest_LegacyInputsString(t *testing.T) {
	body := []byte(`{"inputs":"summarise this passage about photosynthesis"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/models/facebook/bart-large-cnn")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || !strings.Contains(nc.Segments[0], "photosynthesis") {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_LegacyInputsArray(t *testing.T) {
	body := []byte(`{"inputs":["passage one","passage two"]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/models/foo/bar")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 {
		t.Errorf("Segments=%v want 2", nc.Segments)
	}
}

func TestExtractRequest_NoInputsNoMessages(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`{"foo":"bar"}`), "/models/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v", err)
	}
}

func TestExtractRequest_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`not json`), "/models/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v", err)
	}
}

func TestExtractResponse_LegacyArray(t *testing.T) {
	body := []byte(`[{"generated_text":"the answer is 42"}]`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/models/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "the answer is 42" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_LegacySummaryText(t *testing.T) {
	body := []byte(`[{"summary_text":"short summary"}]`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/models/bart")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "short summary" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_ErrorEnvelope(t *testing.T) {
	body := []byte(`{"error":"Model is currently loading"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/models/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Model is currently loading" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_TGIDelta(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"Hello"}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_LegacyTokenText(t *testing.T) {
	chunk := []byte(`{"token":{"text":"the "}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/models/llama")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "the " {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestDetectRequestMeta_HfToken(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api-inference.huggingface.co/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer hf_abcdef")
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "huggingface" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.ApiKeyClass != "huggingface-token" {
		t.Errorf("ApiKeyClass=%q want huggingface-token for hf_ prefix", meta.ApiKeyClass)
	}
}

func TestDetectRequestMeta_NonHfBearer(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api-inference.huggingface.co/x", nil)
	r.Header.Set("Authorization", "Bearer some_other_token")
	meta := a.DetectRequestMeta(r, []byte(`{}`))
	if meta.ApiKeyClass != "huggingface-bearer" {
		t.Errorf("ApiKeyClass=%q want huggingface-bearer for non-hf_ token", meta.ApiKeyClass)
	}
}

func TestDetectResponseUsage_TGI(t *testing.T) {
	body := []byte(`{
		"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":5,"completion_tokens":2}
	}`)
	a := &Adapter{}
	r := &http.Response{Request: &http.Request{}}
	um := a.DetectResponseUsage(r, body)
	if um.PromptTokens == nil || *um.PromptTokens != 5 {
		t.Errorf("PromptTokens=%v", um.PromptTokens)
	}
}

func TestDetectResponseUsage_LegacyParseFailed(t *testing.T) {
	body := []byte(`[{"generated_text":"hi"}]`)
	a := &Adapter{}
	if a.DetectResponseUsage(nil, body).Status != traffic.UsageStatusParseFailed {
		t.Errorf("legacy serverless should yield parse_failed (no usage block)")
	}
}

// Additional coverage: branches not exercised by the original suite.

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"any": "value"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

// hasMessages must return false when the body isn't valid JSON — directly
// covers line 46-48 (the ValidBytes false branch).
func TestHasMessages_InvalidJSON(t *testing.T) {
	if hasMessages([]byte(`not json`)) {
		t.Errorf("hasMessages returned true for non-JSON input")
	}
	if hasMessages([]byte(`{"messages":"not an array"}`)) {
		t.Errorf("hasMessages returned true when messages is not an array")
	}
}

// Legacy request with a top-level `model` string surfaces in Metadata
// (line 77-79).
func TestExtractRequest_LegacyWithModel(t *testing.T) {
	body := []byte(`{"inputs":"summarise","model":"facebook/bart-large-cnn"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/models/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["model"] != "facebook/bart-large-cnn" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

// ExtractResponse: malformed JSON returns ErrMalformed (line 88-90).
func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{not json`), "/models/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// ExtractResponse: object-shaped response with generated_text key
// (line 115-117) — covers the non-array, object branch with a generation key.
func TestExtractResponse_ObjectGeneratedText(t *testing.T) {
	body := []byte(`{"generated_text":"a single answer"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/models/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "a single answer" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// ExtractResponse: array shape but no generation keys → final
// ErrUnknownSchema fall-through (line 124).
func TestExtractResponse_ArrayWithoutGenerationKeys(t *testing.T) {
	body := []byte(`[{"label":"POSITIVE","score":0.99}]`)
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), body, "/models/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// ExtractResponse: object shape with no known keys → ErrUnknownSchema
// (covers the final return after the object branch falls through).
func TestExtractResponse_ObjectUnknown(t *testing.T) {
	body := []byte(`{"foo":"bar"}`)
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), body, "/models/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// ExtractStreamChunk: malformed chunk returns ErrMalformed (line 132-134).
func TestExtractStreamChunk_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractStreamChunk(context.Background(), []byte(`{not json`), "/models/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// DetectRequestMeta: body contains `model` (line 165-167).
func TestDetectRequestMeta_ModelExtracted(t *testing.T) {
	a := &Adapter{}
	meta := a.DetectRequestMeta(nil, []byte(`{"model":"meta-llama/Llama-3"}`))
	if meta.Provider != "huggingface" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "meta-llama/Llama-3" {
		t.Errorf("Model=%q", meta.Model)
	}
}

// DetectResponseUsage: empty body → no_body status (line 178-180).
func TestDetectResponseUsage_EmptyBody(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, nil)
	if um.Status != traffic.UsageStatusNoBody {
		t.Errorf("Status=%v want no_body", um.Status)
	}
}

// Rewrite is unsupported — both methods must return ErrRewriteUnsupported.
func TestRewriteRequestBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/models/x", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 || string(out) != string(body) {
		t.Errorf("body mutated or n!=0")
	}
}

func TestRewriteResponseBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/models/x", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 || string(out) != string(body) {
		t.Errorf("body mutated or n!=0")
	}
}

// Normalize (normalize.go) — delegates to extract.NormalizeForAdapter.
