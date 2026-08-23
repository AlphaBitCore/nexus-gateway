package llm

import (
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestBuildRequestBody_CanonicalMessages_KeepsRecentTurns covers the
// conversation projection: the router-LLM request body contains the
// system prompt plus the recent turns that fit the budget
// (StrategyRecentTurns), each carrying the concatenated text projection
// of its ContentText blocks.
func TestBuildRequestBody_CanonicalMessages_KeepsRecentTurns(t *testing.T) {
	userMsgs := []normalize.Message{
		{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
			{Type: normalize.ContentText, Text: "Hello, write me a haiku."},
		}},
		{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
			{Type: normalize.ContentText, Text: "About spring."},
		}},
	}

	body := BuildRequestBody("router-model-id", Request{
		SystemPrompt: "pick a model",
		Messages:     userMsgs,
	})

	// Both small user turns fit the budget and are kept, oldest first.
	if len(body.Messages) != 3 {
		t.Fatalf("expected 1 system + 2 user turns = 3, got %d: %+v", len(body.Messages), body.Messages)
	}
	if body.Messages[0].Role != "system" {
		t.Errorf("Messages[0].Role = %q, want system", body.Messages[0].Role)
	}
	if body.Messages[1].Content != "Hello, write me a haiku." || body.Messages[2].Content != "About spring." {
		t.Errorf("turns out of order or missing: %+v", body.Messages[1:])
	}
}

// TestBuildRequestBody_EmptyMessages_ReturnsSystemOnly demonstrates that
// the canonical path still produces a valid (if non-grounded) router-LLM
// body when no user content is supplied. A future explicit
// short-circuit upstream of this call would keep the router LLM from being
// invoked in the empty-user-content case; until then the body is
// system-only and the downstream codec (Anthropic) may reject — the
// strategy's smartFallback path then takes over.
func TestBuildRequestBody_EmptyMessages_ReturnsSystemOnly(t *testing.T) {
	body := BuildRequestBody("router-model-id", Request{SystemPrompt: "pick a model"})

	if len(body.Messages) != 1 {
		t.Fatalf("expected only the system message, got %d: %+v", len(body.Messages), body.Messages)
	}
	if body.Messages[0].Role != "system" {
		t.Errorf("Messages[0].Role = %q, want system", body.Messages[0].Role)
	}
}

// TestBuildRequestBody_LongConversation_KeepsAllTurnsThatFit pins the
// inputstaging.Plan(StrategyRecentTurns) behavior: small turns all fit
// the budget and every one reaches the router in chronological order —
// follow-up questions arrive with the context that defines them.
func TestBuildRequestBody_LongConversation_KeepsAllTurnsThatFit(t *testing.T) {
	mk := func(text string) normalize.Message {
		return normalize.Message{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
			{Type: normalize.ContentText, Text: text},
		}}
	}
	userMsgs := []normalize.Message{mk("first"), mk("second"), mk("third"), mk("fourth"), mk("last")}

	body := BuildRequestBody("rm", Request{SystemPrompt: "pick", Messages: userMsgs})

	if len(body.Messages) != 6 {
		t.Fatalf("expected system + all 5 small turns, got %d: %+v", len(body.Messages), body.Messages)
	}
	if body.Messages[0].Role != "system" {
		t.Errorf("Messages[0].Role = %q, want system", body.Messages[0].Role)
	}
	if body.Messages[1].Content != "first" || body.Messages[5].Content != "last" {
		t.Errorf("turns out of order: %+v", body.Messages[1:])
	}
}

// TestBuildRequestBody_MultimodalContent_FlattensTextBlocksOnly shows
// that multimodal request payloads (text + media + tool_use)
// surface only their text projection in the router-LLM prompt — the
// router does not need to see images or tool plumbing to pick a model.
func TestBuildRequestBody_MultimodalContent_FlattensTextBlocksOnly(t *testing.T) {
	userMsgs := []normalize.Message{
		{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
			{Type: normalize.ContentText, Text: "Analyse this:"},
			{Type: normalize.ContentMedia, MediaRef: &normalize.MediaRef{SizeBytes: 1, Mime: "image/png", SHA256: "abc"}},
			{Type: normalize.ContentText, Text: "and tell me the dominant colour."},
		}},
	}

	body := BuildRequestBody("rm", Request{SystemPrompt: "pick", Messages: userMsgs})

	if len(body.Messages) != 2 {
		t.Fatalf("expected 1 system + 1 user, got %d", len(body.Messages))
	}
	want := "Analyse this:\nand tell me the dominant colour."
	if body.Messages[1].Content != want {
		t.Errorf("multimodal text flatten = %q, want %q", body.Messages[1].Content, want)
	}
}

// TestParseResponse_ProviderID covers the optional providerId
// disambiguator returned by router LLMs that need to distinguish models
// sharing a code across providers (e.g. "gpt-4o" on OpenAI vs Azure).
// Moved from router/strategy_smart_catalog_test.go alongside the
// underlying function.
func TestParseResponse_ProviderID(t *testing.T) {
	envelope := `{"choices":[{"message":{"content":"{\"modelId\":\"m1\",\"providerId\":\"p-openai\",\"reason\":\"ok\"}"}}]}`

	d, err := ParseResponse(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if d.ModelID != "m1" || d.ProviderID != "p-openai" || d.Reason != "ok" {
		t.Fatalf("got modelId=%q providerId=%q reason=%q", d.ModelID, d.ProviderID, d.Reason)
	}
}
