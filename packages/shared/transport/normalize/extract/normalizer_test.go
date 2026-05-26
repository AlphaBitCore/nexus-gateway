package extract

import (
	"context"
	"strings"
	"testing"

	normcodecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestPatternNormalizer_ChatGPTWebRequest_TierClaim(t *testing.T) {
	pn := NewPatternNormalizer()
	body := []byte(`{
		"model": "gpt-5-5",
		"messages": [{"author": {"role": "user"}, "content": {"parts": ["hi there"]}}],
		"suggestion_type": "autocomplete",
		"chosen_suggestion": {"index": 0}
	}`)
	payload, err := pn.Normalize(context.Background(), body, normalize.Meta{
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Fatalf("kind: %v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "pattern:chatgpt-web" {
		t.Errorf("detected spec: %q", payload.DetectedSpec)
	}
	if payload.Confidence < 0.7 {
		t.Errorf("confidence: %v", payload.Confidence)
	}
	if payload.Model != "gpt-5-5" {
		t.Errorf("model: %q", payload.Model)
	}
	if len(payload.Messages) != 1 || payload.Messages[0].Role != normalize.RoleUser {
		t.Fatalf("messages: %+v", payload.Messages)
	}
	if !strings.Contains(payload.Messages[0].Content[0].Text, "hi there") {
		t.Errorf("content: %q", payload.Messages[0].Content[0].Text)
	}
	// Dual view: raw bytes preserved in BodyView.Text.
	if payload.HTTP == nil || payload.HTTP.BodyView == nil {
		t.Fatalf("BodyView missing for raw fallback")
	}
	if !strings.Contains(payload.HTTP.BodyView.Text, "gpt-5-5") {
		t.Errorf("raw text lost: %q", payload.HTTP.BodyView.Text)
	}
}

func TestPatternNormalizer_ChatGPTWebResponse_TierClaim(t *testing.T) {
	pn := NewPatternNormalizer()
	raw := []byte(strings.Join([]string{
		"event: delta",
		`data: {"p":"","o":"add","v":{"message":{"author":{"role":"assistant"},"content":{"parts":[""]}},"conversation_id":"c1"}}`,
		"",
		"event: delta",
		`data: {"p":"/message/content/parts/0","o":"append","v":"Hello world"}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"))
	payload, err := pn.Normalize(context.Background(), raw, normalize.Meta{
		Direction:   normalize.DirectionResponse,
		ContentType: "text/event-stream",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Fatalf("kind: %v", payload.Kind)
	}
	if payload.DetectedSpec != "pattern:chatgpt-web" {
		t.Errorf("detected spec: %q", payload.DetectedSpec)
	}
	if !payload.Stream {
		t.Errorf("stream flag not set")
	}
	if len(payload.Messages) != 1 || payload.Messages[0].Role != normalize.RoleAssistant {
		t.Fatalf("messages: %+v", payload.Messages)
	}
	if !strings.Contains(payload.Messages[0].Content[0].Text, "Hello world") {
		t.Errorf("assistant text: %q", payload.Messages[0].Content[0].Text)
	}
}

func TestPatternNormalizer_NonChatJSON_ReturnsErrUnsupported(t *testing.T) {
	// Random JSON below confidence threshold → Coordinator falls through
	// to Tier 3 (the caller's responsibility).
	pn := NewPatternNormalizer()
	body := []byte(`{"foo": "bar", "count": 42}`)
	_, err := pn.Normalize(context.Background(), body, normalize.Meta{
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err == nil {
		t.Fatal("expected ErrUnsupported for non-chat JSON")
	}
}

func TestPatternNormalizer_UsageExtraction_OpenAINonStream(t *testing.T) {
	// "Extract as much as feasible": Usage tokens populated when probe
	// detects them.
	pn := NewPatternNormalizer()
	body := []byte(`{
		"id": "x",
		"choices": [{"message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}],
		"model": "gpt-4o-mini",
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)
	payload, err := pn.Normalize(context.Background(), body, normalize.Meta{
		Direction:   normalize.DirectionResponse,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if payload.Usage == nil {
		t.Fatal("usage not extracted")
	}
	if payload.Usage.PromptTokens == nil || *payload.Usage.PromptTokens != 10 {
		t.Errorf("prompt tokens: %+v", payload.Usage.PromptTokens)
	}
	if payload.Usage.CompletionTokens == nil || *payload.Usage.CompletionTokens != 5 {
		t.Errorf("completion tokens: %+v", payload.Usage.CompletionTokens)
	}
	if payload.FinishReason != "stop" {
		t.Errorf("finish: %q", payload.FinishReason)
	}
}

func TestPatternNormalizer_DirectionInferred(t *testing.T) {
	// When Direction is unset, probe both request and response and pick
	// the higher-confidence detection.
	pn := NewPatternNormalizer()
	body := []byte(`{
		"id": "x",
		"choices": [{"message": {"role": "assistant", "content": "auto-detected"}, "finish_reason": "stop"}],
		"model": "gpt-4o",
		"usage": {"prompt_tokens": 5}
	}`)
	payload, err := pn.Normalize(context.Background(), body, normalize.Meta{
		// Direction left unset
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(payload.Messages[0].Content[0].Text, "auto-detected") {
		t.Errorf("not detected as response: %+v", payload)
	}
}

// Coordinator integration: registry walks Tier 1 → Tier 2 → Tier 3 with
// confidence-aware fall-through.
func TestCoordinator_TierFallThrough_ChatGPTWeb(t *testing.T) {
	reg := normalize.NewRegistry()
	normcodecs.RegisterDefaultAIBuiltins(reg)
	WireTier2(reg)
	reg.Freeze()

	// Pretend chatgpt-web traffic: adapter_type unknown to Tier 1.
	body := []byte(`{
		"model": "gpt-5-5",
		"messages": [{"author": {"role": "user"}, "content": {"parts": ["hello there"]}}],
		"suggestion_type": "autocomplete",
		"chosen_suggestion": {"index": 0}
	}`)
	payload, err := reg.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "chatgpt-web", // not in Tier 1 registrations
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Without Tier 2, this would have ended up in Tier 3 verbatim
	// (Kind=http-json) because chatgpt-web isn't registered. With
	// Tier 2 in place, the pattern probe wins.
	if payload.Kind != normalize.KindAIChat {
		t.Fatalf("kind: %v want ai-chat (Tier 2 should have claimed)", payload.Kind)
	}
	if payload.DetectedSpec != "pattern:chatgpt-web" {
		t.Errorf("spec: %q want pattern:chatgpt-web", payload.DetectedSpec)
	}
	if payload.Model != "gpt-5-5" {
		t.Errorf("model: %q", payload.Model)
	}
}

func TestCoordinator_NonChatHTTP_FallsToTier3(t *testing.T) {
	reg := normalize.NewRegistry()
	normcodecs.RegisterDefaultAIBuiltins(reg)
	WireTier2(reg)
	reg.Freeze()

	// HTTP body that is not chat-shaped: form-encoded credentials, say.
	body := []byte(`username=alice&password=hunter2&token=abc`)
	payload, err := reg.Normalize(context.Background(), body, normalize.Meta{
		Direction:   normalize.DirectionRequest,
		ContentType: "application/x-www-form-urlencoded",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if payload.Kind != normalize.KindHTTPForm {
		t.Fatalf("kind: %v want http-form (Tier 3)", payload.Kind)
	}
}
