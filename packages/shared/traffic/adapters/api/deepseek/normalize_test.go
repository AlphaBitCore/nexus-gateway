package deepseek

import (
	"context"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Normalize: deepseek advertises the openai-chat wire spec, so a body
// shaped like an OpenAI chat completion request must claim Tier-1 with
// DetectedSpec=deepseek and surface the user prompt + model.
func TestNormalize_OpenAIChatShape(t *testing.T) {
	body := []byte(`{
		"model":"deepseek-chat",
		"messages":[{"role":"user","content":"hello deepseek"}]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "deepseek",
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
	if payload.DetectedSpec != "deepseek" {
		t.Errorf("DetectedSpec=%q want deepseek", payload.DetectedSpec)
	}
	if payload.Model != "deepseek-chat" {
		t.Errorf("Model=%q want deepseek-chat", payload.Model)
	}
	if len(payload.Messages) != 1 {
		t.Fatalf("messages=%d want 1", len(payload.Messages))
	}
	if payload.Messages[0].Role != normalize.RoleUser {
		t.Errorf("role=%v want user", payload.Messages[0].Role)
	}
}

// Normalize_NonChatBody: a body that doesn't match the openai-chat shape
// (no messages array, no signature fields) must fail so the coordinator
// advances to Tier 2 / Tier 3 — silently classifying garbage as ai-chat
// would poison downstream cost / cache analytics.
func TestNormalize_NonChatBody(t *testing.T) {
	body := []byte(`{"foo":"bar","count":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "deepseek",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err == nil {
		t.Fatal("expected error for non-chat body")
	}
}

// Normalize_NonStreamResponse: an openai-chat non-streaming response
// shape must classify as ai-chat with DetectedSpec=deepseek and surface
// the assistant turn for downstream rollups.
func TestNormalize_NonStreamResponse(t *testing.T) {
	body := []byte(`{
		"id":"resp_1",
		"object":"chat.completion",
		"model":"deepseek-chat",
		"choices":[
			{"index":0,"message":{"role":"assistant","content":"Hi from deepseek."},"finish_reason":"stop"}
		],
		"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "deepseek",
		Direction:    normalize.DirectionResponse,
		ContentType:  "application/json",
		EndpointPath: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatalf("Normalize err=%v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "deepseek" {
		t.Errorf("DetectedSpec=%q want deepseek", payload.DetectedSpec)
	}
}
