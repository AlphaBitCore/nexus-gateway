package openaiplatformweb

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Configure — delegates to inner with nil + map; no-op shape.

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

// RewriteRequestBody — delegates to inner openai-compat rewriter.

func TestRewriteRequestBody_Delegation(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"email a@b.com"}]}`)
	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"email [REDACTED]"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n < 1 {
		t.Errorf("n=%d want >=1", n)
	}
	if !strings.Contains(string(rewritten), "[REDACTED]") {
		t.Errorf("rewritten did not include [REDACTED]: %s", rewritten)
	}
}

// RewriteResponseBody — delegates to inner openai-compat rewriter.

func TestRewriteResponseBody_Delegation(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"call me 555-1212"}}]}`)
	a := &Adapter{}
	rewritten, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/chat/completions",
		traffic.NormalizedContent{Segments: []string{"call me [REDACTED]"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n < 1 {
		t.Errorf("n=%d want >=1", n)
	}
	if !strings.Contains(string(rewritten), "[REDACTED]") {
		t.Errorf("rewritten did not include [REDACTED]: %s", rewritten)
	}
}

// DetectResponseUsage — delegates to inner openai-compat usage detector.

func TestDetectResponseUsage_Delegation(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"usage":{"prompt_tokens":42,"completion_tokens":17,"total_tokens":59}}`)
	usage := a.DetectResponseUsage(nil, body)
	if usage.Status != traffic.UsageStatusOK {
		t.Errorf("status=%q want ok", usage.Status)
	}
	if usage.PromptTokens == nil || *usage.PromptTokens != 42 {
		t.Errorf("PromptTokens=%v want 42", usage.PromptTokens)
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 17 {
		t.Errorf("CompletionTokens=%v want 17", usage.CompletionTokens)
	}
}

// Normalize — Tier-1 dispatch via the unified extract helper.

func TestNormalize_OpenAIChatShape(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o",
		"messages":[
			{"role":"system","content":"You are a helpful playground assistant."},
			{"role":"user","content":"hello from platform.openai.com"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "openai-platform-web",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatalf("Normalize err=%v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "openai-platform-web" {
		t.Errorf("DetectedSpec=%q want openai-platform-web", payload.DetectedSpec)
	}
	if payload.Model != "gpt-4o" {
		t.Errorf("Model=%q want gpt-4o", payload.Model)
	}
}

func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), []byte(`{"foo":"bar","baz":42}`), normalize.Meta{
		AdapterType: "openai-platform-web",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Errorf("err=%v want ErrUnsupported", err)
	}
}

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "openai-platform-web" {
		t.Errorf("ID=%q", a.ID())
	}
}

func TestExtractRequest_PlaygroundChatCompletions(t *testing.T) {
	// Playground uses standard /v1/chat/completions wire.
	body := []byte(`{
		"model":"gpt-4o",
		"messages":[{"role":"user","content":"hi from playground"}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hi from playground" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "gpt-4o" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

func TestExtractRequest_ToolCallsDelegation(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":null,"tool_calls":[
		{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}
	]}]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"f"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractResponse_FinishReasonDelegation(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o",
		"system_fingerprint":"fp_abc",
		"choices":[{"message":{"role":"assistant","content":"hi"}, "finish_reason":"stop"}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%q", nc.Metadata["finish_reason"])
	}
	if nc.Metadata["system_fingerprint"] != "fp_abc" {
		t.Errorf("system_fingerprint=%q", nc.Metadata["system_fingerprint"])
	}
}

func TestExtractStreamChunk_Delegation(t *testing.T) {
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

func TestDetectRequestMeta_ProviderRelabel(t *testing.T) {
	// Provider must be re-labelled "openai-platform-web", not the inner
	// adapter's default "openai-compat" provider.
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://platform.openai.com/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-proj-abc")
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "openai-platform-web" {
		t.Errorf("Provider=%q want openai-platform-web", meta.Provider)
	}
	if meta.Model != "gpt-4o" {
		t.Errorf("Model=%q", meta.Model)
	}
}

func TestNormalisePath_Identity(t *testing.T) {
	for _, p := range []string{"/v1/chat/completions", "/v1/responses", "/v1/embeddings", "/dashboard"} {
		if got := normalisePath(p); got != p {
			t.Errorf("normalisePath(%q)=%q want identity", p, got)
		}
	}
}
