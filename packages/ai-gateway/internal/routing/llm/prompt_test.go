package llm

import (
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestBuildRequestBody_CanonicalMessages_FiltersToUserRole covers the
// message-filtering fix: when the smart strategy hands a mixed-role
// []normalize.Message slice, the router-LLM request body must contain
// the system prompt plus only the last user message (StrategyLastUser).
// System / assistant / tool messages from the original request are
// dropped, and the surviving user message carries the concatenated text
// projection of its ContentText blocks.
func TestBuildRequestBody_CanonicalMessages_FiltersToUserRole(t *testing.T) {
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
		UserMessages: userMsgs,
	})

	// StrategyLastUser: system + only the last user message.
	if len(body.Messages) != 2 {
		t.Fatalf("expected 1 system + 1 user (last) = 2, got %d: %+v", len(body.Messages), body.Messages)
	}
	if body.Messages[0].Role != "system" {
		t.Errorf("Messages[0].Role = %q, want system", body.Messages[0].Role)
	}
	if body.Messages[1].Role != "user" {
		t.Errorf("Messages[1].Role = %q, want user", body.Messages[1].Role)
	}
	// "About spring." is the last user message — it must be kept.
	if body.Messages[1].Content != "About spring." {
		t.Errorf("Messages[1].Content = %q, want %q", body.Messages[1].Content, "About spring.")
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

// TestBuildRequestBody_LongConversation_KeepsOnlyLastUser pins the
// inputstaging.Plan(StrategyLastUser) truncation behavior: when the user's
// history is long, the router LLM receives only the most recent user
// message — it is a classification task that needs only the immediate
// question, not the full conversation history.
func TestBuildRequestBody_LongConversation_KeepsOnlyLastUser(t *testing.T) {
	mk := func(text string) normalize.Message {
		return normalize.Message{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
			{Type: normalize.ContentText, Text: text},
		}}
	}
	userMsgs := []normalize.Message{mk("first"), mk("second"), mk("third"), mk("fourth"), mk("last")}

	body := BuildRequestBody("rm", Request{SystemPrompt: "pick", UserMessages: userMsgs})

	// system + exactly one user message (the last one).
	if len(body.Messages) != 2 {
		t.Fatalf("truncation: expected 2 entries (system + last-user), got %d: %+v",
			len(body.Messages), body.Messages)
	}
	if body.Messages[0].Role != "system" {
		t.Errorf("Messages[0].Role = %q, want system", body.Messages[0].Role)
	}
	if body.Messages[1].Role != "user" {
		t.Errorf("Messages[1].Role = %q, want user", body.Messages[1].Role)
	}
	if body.Messages[1].Content != "last" {
		t.Errorf("Messages[1].Content = %q, want %q (last user message)", body.Messages[1].Content, "last")
	}
}

// TestBuildRequestBody_MultimodalContent_FlattensTextBlocksOnly shows
// that multimodal request payloads (text + image_ref + tool_use)
// surface only their text projection in the router-LLM prompt — the
// router does not need to see images or tool plumbing to pick a model.
func TestBuildRequestBody_MultimodalContent_FlattensTextBlocksOnly(t *testing.T) {
	userMsgs := []normalize.Message{
		{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
			{Type: normalize.ContentText, Text: "Analyse this:"},
			{Type: normalize.ContentImageRef, ImageRef: &normalize.BinaryRef{Size: 1, ContentType: "image/png", SHA256: "abc"}},
			{Type: normalize.ContentText, Text: "and tell me the dominant colour."},
		}},
	}

	body := BuildRequestBody("rm", Request{SystemPrompt: "pick", UserMessages: userMsgs})

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
