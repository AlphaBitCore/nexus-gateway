package tabnine

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
	if a.ID() != "tabnine" {
		t.Errorf("ID=%q", a.ID())
	}
}

func TestExtractRequest_OpenAICompatMessages(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"complete this function"}],"model":"tabnine-protected"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "complete this function" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "tabnine-protected" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

func TestExtractRequest_PromptShape(t *testing.T) {
	body := []byte(`{"prompt":"refactor this code","conversation_id":"c-1"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "refactor this code" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_ToolCalls(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":null,"tool_calls":[
		{"id":"c1","type":"function","function":{"name":"refactor","arguments":"{}"}}
	]}]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"refactor"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractRequest_BinaryBody(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte{0x00, 0x42}, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v", err)
	}
}

func TestExtractStreamChunk_OpenAICompat(t *testing.T) {
	chunk := []byte(`{"choices":[{"delta":{"content":"Hello"}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Hello" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_ErrorEnvelope(t *testing.T) {
	body := []byte(`{"error":{"message":"unauthorized"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "unauthorized" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestDetectRequestMeta_BearerToken(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api.tabnine.com/api/chat", nil)
	r.Header.Set("Authorization", "Bearer tabnine_service_key_xyz")
	meta := a.DetectRequestMeta(r, []byte(`{"model":"tabnine-protected"}`))
	if meta.Provider != "tabnine" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.ApiKeyClass != "tabnine-bearer" {
		t.Errorf("ApiKeyClass=%q", meta.ApiKeyClass)
	}
	if meta.ApiKeyFingerprint == "" {
		t.Errorf("ApiKeyFingerprint should be set")
	}
}

func TestDetectResponseUsage_NonLLM(t *testing.T) {
	a := &Adapter{}
	if a.DetectResponseUsage(nil, []byte(`{}`)).Status != traffic.UsageStatusNonLLM {
		t.Errorf("want non_llm")
	}
}

func TestRewrite_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{}`)
	if _, _, err := a.RewriteRequestBody(context.Background(), body, "/x", traffic.NormalizedContent{}); !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("Request rewrite err=%v", err)
	}
	if _, _, err := a.RewriteResponseBody(context.Background(), body, "/x", traffic.NormalizedContent{}); !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("Response rewrite err=%v", err)
	}
}
