package llm

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/inputstaging"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Router-input budget contract: the user-content budget is
//
//	min(routerContextLimit − EstimateTokens(systemPrompt) − routerReserveOutput, routerInputCap)
//
// floored at routerMinInputBudget, staged with StrategyRecentTurns under
// default budget enforcement — the router call can never carry an
// over-limit prompt, and assistant turns ride along for follow-up
// context. routerContextLimit is the router model's real declared window
// (Request.RouterContextLimit); 8192 is only the undeclared fallback.

func textMsgLLM(role normcore.Role, text string) normcore.Message {
	return normcore.Message{Role: role, Content: []normcore.ContentBlock{{Type: normcore.ContentText, Text: text}}}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// estimateBodyUserTokens sums the estimated tokens of all non-system
// messages in the built body.
func estimateBodyUserTokens(b requestBody) int {
	total := 0
	for _, m := range b.Messages {
		if m.Role != "system" {
			total += inputstaging.EstimateTokens(m.Content)
		}
	}
	return total
}

func TestBuildRequestBody_RealRouterWindowShrinksBudget(t *testing.T) {
	sys := strings.Repeat("s", 400) // ≈100 tokens
	huge := strings.Repeat("a", 12000)
	req := Request{
		SystemPrompt:       sys,
		Messages:           []normcore.Message{textMsgLLM(normcore.RoleUser, huge)},
		RouterContextLimit: 1000,
	}
	body := buildRequestBodyWithLogger("m", req, discard())
	// budget = 1000 − 100 − 256 = 644
	if got := estimateBodyUserTokens(body); got > 644 {
		t.Errorf("user content estimates %d tokens, budget 644 (limit 1000 − sys 100 − reserve 256)", got)
	}
	if got := estimateBodyUserTokens(body); got == 0 {
		t.Errorf("user content must not be empty")
	}
}

func TestBuildRequestBody_UndeclaredWindowFallsBackTo8192(t *testing.T) {
	msg := strings.Repeat("a", 8000) // ≈2000 tokens — fits an 8192 window, not a 1000 one
	req := Request{
		SystemPrompt: "route it",
		Messages:     []normcore.Message{textMsgLLM(normcore.RoleUser, msg)},
	}
	body := buildRequestBodyWithLogger("m", req, discard())
	found := false
	for _, m := range body.Messages {
		if m.Role == "user" && m.Content == msg {
			found = true
		}
	}
	if !found {
		t.Errorf("2000-token message must pass through untouched under the 8192 fallback window")
	}
}

func TestBuildRequestBody_InputCappedAt4096(t *testing.T) {
	huge := strings.Repeat("a", 40000) // ≈10000 tokens
	req := Request{
		SystemPrompt:       "route it",
		Messages:           []normcore.Message{textMsgLLM(normcore.RoleUser, huge)},
		RouterContextLimit: 200000, // giant router window must not inflate router cost
	}
	body := buildRequestBodyWithLogger("m", req, discard())
	if got := estimateBodyUserTokens(body); got > 4096 {
		t.Errorf("user content estimates %d tokens; router input is capped at 4096", got)
	}
}

func TestBuildRequestBody_SystemPromptEatingWindow_FloorsAndStillSends(t *testing.T) {
	sys := strings.Repeat("s", 4000) // ≈1000 tokens > 1000-token window − reserve
	req := Request{
		SystemPrompt:       sys,
		Messages:           []normcore.Message{textMsgLLM(normcore.RoleUser, strings.Repeat("a", 8000))},
		RouterContextLimit: 1000,
	}
	body := buildRequestBodyWithLogger("m", req, discard())
	got := estimateBodyUserTokens(body)
	if got == 0 {
		t.Errorf("floor must still send some user content")
	}
	if got > 256 {
		t.Errorf("user content estimates %d tokens, floor budget is 256", got)
	}
}

func TestBuildRequestBody_RecentTurnsPreservesAssistantRole(t *testing.T) {
	req := Request{
		SystemPrompt: "route it",
		Messages: []normcore.Message{
			textMsgLLM(normcore.RoleUser, "write a haiku about spring"),
			textMsgLLM(normcore.RoleAssistant, "cherry petals drift"),
			textMsgLLM(normcore.RoleUser, "another one"),
		},
		RouterContextLimit: 8192,
	}
	body := buildRequestBodyWithLogger("m", req, discard())
	var roles []string
	for _, m := range body.Messages {
		if m.Role != "system" {
			roles = append(roles, m.Role)
		}
	}
	want := []string{"user", "assistant", "user"}
	if len(roles) != len(want) {
		t.Fatalf("got %d conversation messages %v, want %v", len(roles), roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("role[%d] = %q, want %q (assistant turns must reach the router)", i, roles[i], want[i])
		}
	}
}

// The pre-fix hazard: a single user message exceeding the budget was
// logged and sent AS-IS. It must now be truncated before the call.
func TestBuildRequestBody_OversizedSingleMessageIsCutNotSentAsIs(t *testing.T) {
	huge := strings.Repeat("a", 100000) // ≈25000 tokens
	req := Request{
		SystemPrompt:       "route it",
		Messages:           []normcore.Message{textMsgLLM(normcore.RoleUser, huge)},
		RouterContextLimit: 8192,
	}
	body := buildRequestBodyWithLogger("m", req, discard())
	for _, m := range body.Messages {
		if m.Role == "user" && m.Content == huge {
			t.Fatalf("oversized message was sent as-is; it must be truncated to the budget")
		}
	}
	if got := estimateBodyUserTokens(body); got > 4096 {
		t.Errorf("user content estimates %d tokens, exceeds the capped budget", got)
	}
}

// TestBuildRequestBody_NoUserText_FallsBackToSystemOnly: when every
// user turn projects to empty text (image-only) the conversation would
// start with an assistant turn — Anthropic-shape routers reject
// assistant-first conversations, so the builder must degrade to the
// system-only body (the pre-existing no-content shape) instead.
func TestBuildRequestBody_NoUserText_FallsBackToSystemOnly(t *testing.T) {
	req := Request{
		SystemPrompt: "route it",
		Messages: []normcore.Message{
			{Role: normcore.RoleUser, Content: []normcore.ContentBlock{
				{Type: normcore.ContentMedia, MediaRef: &normcore.MediaRef{SizeBytes: 1, Mime: "image/png", SHA256: "a"}},
			}},
			textMsgLLM(normcore.RoleAssistant, "I see a cat in the image"),
		},
		RouterContextLimit: 8192,
	}
	body := buildRequestBodyWithLogger("m", req, discard())
	if len(body.Messages) != 1 || body.Messages[0].Role != "system" {
		t.Fatalf("expected system-only body when no user text survives projection, got %+v", body.Messages)
	}
}

// TestBuildRequestBody_LeadingAssistantTrimmed: an unpaired assistant
// turn surviving ahead of the first user turn (its user partner
// projected empty) must be trimmed so the conversation starts with a
// user message.
func TestBuildRequestBody_LeadingAssistantTrimmed(t *testing.T) {
	req := Request{
		SystemPrompt: "route it",
		Messages: []normcore.Message{
			{Role: normcore.RoleUser, Content: []normcore.ContentBlock{
				{Type: normcore.ContentMedia, MediaRef: &normcore.MediaRef{SizeBytes: 1, Mime: "image/png", SHA256: "a"}},
			}}, // projects empty — its assistant reply becomes unpaired
			textMsgLLM(normcore.RoleAssistant, "it is a cat"),
			textMsgLLM(normcore.RoleUser, "what breed?"),
		},
		RouterContextLimit: 8192,
	}
	body := buildRequestBodyWithLogger("m", req, discard())
	if len(body.Messages) < 2 {
		t.Fatalf("expected conversation after system, got %+v", body.Messages)
	}
	if body.Messages[1].Role != "user" {
		t.Fatalf("conversation must start with a user turn, got %q first", body.Messages[1].Role)
	}
}
