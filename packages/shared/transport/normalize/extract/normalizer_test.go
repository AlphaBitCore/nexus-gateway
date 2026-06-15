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
		`data: {"type":"message_marker","conversation_id":"c1","message_id":"m1"}`,
		"",
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

func TestPatternNormalizer_StandardOpenAIResponse_NotClaimed(t *testing.T) {
	// The Tier-2 probe no longer recognises standard-API response
	// wires — the Tier-1 codecs decode them. A canonical OpenAI
	// non-stream response must fall through with ErrUnsupported.
	pn := NewPatternNormalizer()
	body := []byte(`{
		"id": "x",
		"choices": [{"message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}],
		"model": "gpt-4o-mini",
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)
	if _, err := pn.Normalize(context.Background(), body, normalize.Meta{
		Direction:   normalize.DirectionResponse,
		ContentType: "application/json",
	}); err == nil {
		t.Fatal("expected ErrUnsupported — standard OpenAI responses are codec territory")
	}
}

func TestPatternNormalizer_DirectionInferred(t *testing.T) {
	// When Direction is unset, probe both request and response and pick
	// the higher-confidence detection — here a chatgpt-web SSE response
	// body (the request probe scores zero on SSE bytes).
	pn := NewPatternNormalizer()
	body := []byte(strings.Join([]string{
		`data: {"type":"message_marker","conversation_id":"c1","message_id":"m1"}`,
		"",
		"event: delta",
		`data: {"p":"","o":"add","v":{"message":{"author":{"role":"assistant"},"content":{"parts":[""]}}}}`,
		"",
		"event: delta",
		`data: {"p":"/message/content/parts/0","o":"append","v":"auto-detected"}`,
		"",
	}, "\n"))
	payload, err := pn.Normalize(context.Background(), body, normalize.Meta{
		// Direction left unset
		ContentType: "text/event-stream",
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

// TestNormalizeForAdapter_DirectionUnsetRequestWins covers the
// direction-unset arm where the REQUEST probe out-scores the response
// probe: a chatgpt-web JSON request body scores zero on the SSE-only
// response probe, so the request detection must win and claim.
func TestNormalizeForAdapter_DirectionUnsetRequestWins(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5-5",
		"messages": [{"author": {"role": "user"}, "content": {"parts": ["request wins"]}}],
		"suggestion_type": "autocomplete"
	}`)
	out, err := NormalizeForAdapter(body, normalize.Meta{}, AdapterSpecHint{
		AdapterID:     "chatgpt-web",
		ReqSpecIDs:    []string{"chatgpt-web"},
		RespSpecIDs:   []string{"chatgpt-web"},
		MinConfidence: 0.5,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out.Messages) != 1 || !strings.Contains(out.Messages[0].Content[0].Text, "request wins") {
		t.Errorf("request-direction detection did not win: %+v", out.Messages)
	}
}
