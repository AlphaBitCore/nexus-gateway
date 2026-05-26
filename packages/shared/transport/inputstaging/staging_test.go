package inputstaging

import (
	"errors"
	"testing"
)

// --- helpers ---

func msg(role, content string) Message {
	return Message{Role: role, Content: content}
}

// makeConversation builds a synthetic conversation:
// 1 system + alternating user/assistant pairs.
// Each message's content is repeated to produce the requested token count.
// The heuristic is: each "a" character = 0.25 tokens, so
// content = strings.Repeat("a", tokCount*4) yields ~tokCount tokens.
func tokContent(n int) string {
	// n*4 ASCII chars → n*4*0.25 = n tokens exactly.
	buf := make([]byte, n*4)
	for i := range buf {
		buf[i] = 'a'
	}
	return string(buf)
}

// --- Strategy.Valid ---

func TestStrategyValid(t *testing.T) {
	valid := []Strategy{
		StrategyLastUser,
		StrategySystemPlusLastUser,
		StrategyRecentTurns,
		StrategyHeadPlusTail,
		StrategyFullTruncated,
	}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("Strategy(%q).Valid() = false, want true", s)
		}
	}
	invalid := []Strategy{"", "unknown", "LAST_USER"}
	for _, s := range invalid {
		if s.Valid() {
			t.Errorf("Strategy(%q).Valid() = true, want false", s)
		}
	}
}

// --- ErrInvalidStrategy ---

func TestPlan_InvalidStrategy(t *testing.T) {
	_, err := Plan(PlanInput{
		Messages:          []Message{msg("user", "hello")},
		ModelContextLimit: 1000,
		Strategy:          "bogus",
		ReserveOutput:     0,
	})
	if !errors.Is(err, ErrInvalidStrategy) {
		t.Errorf("Plan with invalid strategy: got %v, want ErrInvalidStrategy", err)
	}
}

func TestPlan_InvalidModelContextLimit(t *testing.T) {
	_, err := Plan(PlanInput{
		Messages:          []Message{msg("user", "hello")},
		ModelContextLimit: 0,
		Strategy:          StrategyLastUser,
	})
	if err == nil {
		t.Error("Plan with ModelContextLimit=0: want error, got nil")
	}
}

// --- StrategyLastUser ---

func TestPlanLastUser_NoUserMessage(t *testing.T) {
	res, err := Plan(PlanInput{
		Messages:          []Message{msg("system", "You are helpful."), msg("assistant", "Hi")},
		ModelContextLimit: 1000,
		Strategy:          StrategyLastUser,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.OverflowKind != OverflowAfterStrategy {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowAfterStrategy)
	}
	if len(res.Messages) != 0 {
		t.Errorf("Messages len = %d, want 0", len(res.Messages))
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true")
	}
}

func TestPlanLastUser_Fits(t *testing.T) {
	msgs := []Message{
		msg("system", tokContent(10)),
		msg("user", tokContent(5)),
		msg("assistant", tokContent(5)),
		msg("user", tokContent(10)), // last user
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategyLastUser,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.OverflowKind != OverflowNone {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowNone)
	}
	if len(res.Messages) != 1 || res.Messages[0].Role != "user" {
		t.Errorf("Messages = %v, want single user message", res.Messages)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true (3 messages dropped)")
	}
	if res.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", res.InputTokens)
	}
}

func TestPlanLastUser_SingleMessageTooBig(t *testing.T) {
	res, err := Plan(PlanInput{
		Messages:          []Message{msg("user", tokContent(100))},
		ModelContextLimit: 50,
		Strategy:          StrategyLastUser,
		ReserveOutput:     10,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// budget = 50 - 10 = 40; message is 100 tokens.
	if res.OverflowKind != OverflowSingleMessageTooBig {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowSingleMessageTooBig)
	}
}

func TestPlanLastUser_ExactlyOnlyUserMessage(t *testing.T) {
	// Single user message, no truncation needed.
	res, err := Plan(PlanInput{
		Messages:          []Message{msg("user", tokContent(10))},
		ModelContextLimit: 100,
		Strategy:          StrategyLastUser,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Truncated {
		t.Error("Truncated = true, want false (only message is the user message)")
	}
	if res.OverflowKind != OverflowNone {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowNone)
	}
}

// --- StrategySystemPlusLastUser ---

func TestPlanSystemPlusLastUser_Basic(t *testing.T) {
	msgs := []Message{
		msg("system", tokContent(10)),
		msg("user", tokContent(5)),
		msg("assistant", tokContent(5)),
		msg("user", tokContent(10)),
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategySystemPlusLastUser,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.OverflowKind != OverflowNone {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowNone)
	}
	// Should have system + last user = 2 messages.
	if len(res.Messages) != 2 {
		t.Errorf("Messages len = %d, want 2", len(res.Messages))
	}
	if res.Messages[0].Role != "system" {
		t.Errorf("first message role = %q, want system", res.Messages[0].Role)
	}
	if res.Messages[1].Role != "user" {
		t.Errorf("second message role = %q, want user", res.Messages[1].Role)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true")
	}
}

func TestPlanSystemPlusLastUser_MultipleSystemMessages(t *testing.T) {
	msgs := []Message{
		msg("system", tokContent(5)),
		msg("system", tokContent(5)),
		msg("user", tokContent(5)),
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategySystemPlusLastUser,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Messages) != 3 {
		t.Errorf("Messages len = %d, want 3 (both system + user)", len(res.Messages))
	}
	// Not truncated: all 3 messages in input map directly to the 3 returned messages.
	if res.Truncated {
		t.Error("Truncated = true, want false")
	}
}

func TestPlanSystemPlusLastUser_NoUserMessage(t *testing.T) {
	msgs := []Message{
		msg("system", tokContent(5)),
		msg("assistant", tokContent(5)),
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategySystemPlusLastUser,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.OverflowKind != OverflowAfterStrategy {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowAfterStrategy)
	}
}

func TestPlanSystemPlusLastUser_LastUserTooBig(t *testing.T) {
	msgs := []Message{
		msg("system", tokContent(5)),
		msg("user", tokContent(200)),
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategySystemPlusLastUser,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.OverflowKind != OverflowSingleMessageTooBig {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowSingleMessageTooBig)
	}
}

func TestPlanSystemPlusLastUser_SystemPushesTooBig(t *testing.T) {
	// System messages make total > budget but user alone fits.
	msgs := []Message{
		msg("system", tokContent(80)),
		msg("user", tokContent(30)),
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategySystemPlusLastUser,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// system(80) + user(30) = 110 > 100; but user(30) < 100.
	// Should return overflow=after_strategy and just the user message.
	if res.OverflowKind != OverflowAfterStrategy {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowAfterStrategy)
	}
	if len(res.Messages) != 1 || res.Messages[0].Role != "user" {
		t.Errorf("Messages = %v, want single user message", res.Messages)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true")
	}
}

// --- StrategyRecentTurns ---

func TestPlanRecentTurns_FitsAll(t *testing.T) {
	msgs := []Message{
		msg("system", tokContent(10)),
		msg("user", tokContent(5)),
		msg("assistant", tokContent(5)),
		msg("user", tokContent(5)),
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategyRecentTurns,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.OverflowKind != OverflowNone {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowNone)
	}
	if res.Truncated {
		t.Error("Truncated = true, want false")
	}
	if len(res.Messages) != 4 {
		t.Errorf("Messages len = %d, want 4", len(res.Messages))
	}
}

func TestPlanRecentTurns_DropOldTurns(t *testing.T) {
	msgs := []Message{
		msg("system", tokContent(10)),
		msg("user", tokContent(30)),      // old turn — should be dropped
		msg("assistant", tokContent(30)), // old turn — should be dropped
		msg("user", tokContent(20)),      // recent turn — should be kept
		msg("assistant", tokContent(20)), // recent turn — should be kept
		msg("user", tokContent(10)),      // last turn
	}
	// budget = 100; system=10 → body budget=90.
	// Walk from end: user(10)+assistant(20)+user(20) = 50 ≤ 90 → keep.
	// assistant(30)+user(30) would push to 50+60=110 > 90 → stop.
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategyRecentTurns,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.OverflowKind != OverflowNone {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowNone)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true")
	}
	// system + 3 recent body messages = 4 messages.
	if len(res.Messages) != 4 {
		t.Errorf("Messages len = %d, want 4", len(res.Messages))
	}
	if res.Messages[0].Role != "system" {
		t.Errorf("first message role = %q, want system", res.Messages[0].Role)
	}
}

func TestPlanRecentTurns_EmptyConversation(t *testing.T) {
	res, err := Plan(PlanInput{
		Messages:          []Message{},
		ModelContextLimit: 100,
		Strategy:          StrategyRecentTurns,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Messages) != 0 {
		t.Errorf("Messages len = %d, want 0", len(res.Messages))
	}
	if res.Truncated {
		t.Error("Truncated = true, want false for empty input")
	}
}

// --- StrategyHeadPlusTail ---

func TestPlanHeadPlusTail_ShortFitsAll(t *testing.T) {
	msgs := []Message{
		msg("system", tokContent(5)),
		msg("user", tokContent(5)),
		msg("assistant", tokContent(5)),
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategyHeadPlusTail,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.OverflowKind != OverflowNone {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowNone)
	}
}

func TestPlanHeadPlusTail_LongConversation(t *testing.T) {
	// Budget tight enough to force middle messages to be dropped.
	// system=10, bodyBudget=20 (budget=30-10), headBudget=6 (30%), tailBudget=14 (70%).
	// head: user(5) fits (5 ≤ 6), assistant(5) would push to 10 > 6 → only user kept in head.
	// tail: walks from bodyMsgs end (indices 5,4 relative to body = assistant(5), user(5)).
	//   assistant(5) at body-end: 5 ≤ 14 → keep; user(5) next: 10 ≤ 14 → keep.
	//   Body index headLen(1) now reached, stop.
	// Middle messages (assistant(5) at body[1], user(5) at body[2], assistant(5) at body[3])
	// may or may not be dropped depending on budget arithmetic, but with 7 input messages
	// and only head+tail selected, at least 1 is dropped.
	msgs := []Message{
		msg("system", tokContent(10)),
		msg("user", tokContent(5)),      // body[0]: head candidate
		msg("assistant", tokContent(5)), // body[1]: middle candidate
		msg("user", tokContent(5)),      // body[2]: middle candidate
		msg("assistant", tokContent(5)), // body[3]: middle candidate
		msg("user", tokContent(5)),      // body[4]: tail candidate
		msg("assistant", tokContent(5)), // body[5]: tail candidate
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 30,
		Strategy:          StrategyHeadPlusTail,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// With a 30-token budget and 7 messages totaling 40 tokens,
	// at least some middle messages must be dropped.
	if !res.Truncated {
		t.Error("Truncated = false, want true (middle messages dropped)")
	}
	// Verify system is always present.
	if len(res.Messages) == 0 || res.Messages[0].Role != "system" {
		t.Errorf("expected system as first message, got: %v", res.Messages)
	}
}

func TestPlanHeadPlusTail_NoBody(t *testing.T) {
	msgs := []Message{
		msg("system", tokContent(5)),
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategyHeadPlusTail,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.OverflowKind != OverflowNone {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowNone)
	}
}

// --- StrategyFullTruncated ---

func TestPlanFullTruncated_Fits(t *testing.T) {
	msgs := []Message{
		msg("system", tokContent(10)),
		msg("user", tokContent(20)),
		msg("assistant", tokContent(20)),
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategyFullTruncated,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.OverflowKind != OverflowNone {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowNone)
	}
	if res.Truncated {
		t.Error("Truncated = true, want false")
	}
	if len(res.Messages) != 3 {
		t.Errorf("Messages len = %d, want 3", len(res.Messages))
	}
}

func TestPlanFullTruncated_OverBudget(t *testing.T) {
	msgs := []Message{
		msg("system", tokContent(10)),
		msg("user", tokContent(50)),
		msg("assistant", tokContent(50)),
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategyFullTruncated,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 10+50+50=110 > 100; last message is 50 ≤ 100.
	if res.OverflowKind != OverflowAfterStrategy {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowAfterStrategy)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true")
	}
	// Content unchanged.
	if len(res.Messages) != len(msgs) {
		t.Errorf("Messages len = %d, want %d (full_truncated does not cut content)", len(res.Messages), len(msgs))
	}
}

func TestPlanFullTruncated_SingleMessageTooBig(t *testing.T) {
	msgs := []Message{
		msg("user", tokContent(200)),
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategyFullTruncated,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.OverflowKind != OverflowSingleMessageTooBig {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowSingleMessageTooBig)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true")
	}
}

func TestPlanFullTruncated_Empty(t *testing.T) {
	res, err := Plan(PlanInput{
		Messages:          []Message{},
		ModelContextLimit: 100,
		Strategy:          StrategyFullTruncated,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.OverflowKind != OverflowNone {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowNone)
	}
	if res.Truncated {
		t.Error("Truncated = true, want false")
	}
}

// --- Suggest ---

func TestSuggest(t *testing.T) {
	tests := []struct {
		name              string
		modelContextLimit int
		profile           Profile
		want              Strategy
	}{
		{"tiny_any_generic", 512, ProfileGeneric, StrategyLastUser},
		{"tiny_any_short", 1024, ProfileShortAnswer, StrategyLastUser},
		{"tiny_any_long", 1024, ProfileLongCompletion, StrategyLastUser},
		{"small_generic", 2048, ProfileGeneric, StrategySystemPlusLastUser},
		{"small_short", 4096, ProfileShortAnswer, StrategySystemPlusLastUser},
		{"small_long", 2048, ProfileLongCompletion, StrategyLastUser},
		{"small_long_at_boundary", 4096, ProfileLongCompletion, StrategyLastUser},
		{"medium_generic", 8192, ProfileGeneric, StrategySystemPlusLastUser},
		{"medium_short", 8192, ProfileShortAnswer, StrategySystemPlusLastUser},
		{"medium_long", 8192, ProfileLongCompletion, StrategyRecentTurns},
		{"medium_at_boundary", 16384, ProfileLongCompletion, StrategyRecentTurns},
		{"large_generic", 32768, ProfileGeneric, StrategyRecentTurns},
		{"large_short", 128000, ProfileShortAnswer, StrategyRecentTurns},
		{"large_long", 200000, ProfileLongCompletion, StrategyRecentTurns},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Suggest(tc.modelContextLimit, tc.profile)
			if got != tc.want {
				t.Errorf("Suggest(%d, %q) = %q, want %q", tc.modelContextLimit, tc.profile, got, tc.want)
			}
		})
	}
}

// --- ReserveOutput budget ---

func TestPlan_ReserveOutput(t *testing.T) {
	// ModelContextLimit=100, ReserveOutput=60 → budget=40.
	// Message is 50 tokens → SingleMessageTooBig.
	res, err := Plan(PlanInput{
		Messages:          []Message{msg("user", tokContent(50))},
		ModelContextLimit: 100,
		Strategy:          StrategyLastUser,
		ReserveOutput:     60,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.OverflowKind != OverflowSingleMessageTooBig {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowSingleMessageTooBig)
	}
}

func TestPlan_ReserveOutputGreaterThanLimit(t *testing.T) {
	// budget = max(10 - 20, 0) = 0. Any non-empty message → overflow.
	res, err := Plan(PlanInput{
		Messages:          []Message{msg("user", "hi")},
		ModelContextLimit: 10,
		Strategy:          StrategyLastUser,
		ReserveOutput:     20,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.OverflowKind != OverflowSingleMessageTooBig {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowSingleMessageTooBig)
	}
}

// --- InputTokens accounting ---

func TestPlan_InputTokensAccuracy(t *testing.T) {
	// system=10, user=20 → total=30.
	msgs := []Message{
		msg("system", tokContent(10)),
		msg("user", tokContent(20)),
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategySystemPlusLastUser,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.InputTokens != 30 {
		t.Errorf("InputTokens = %d, want 30", res.InputTokens)
	}
}

// --- Tool role messages ---

func TestPlanLastUser_IgnoresToolMessages(t *testing.T) {
	msgs := []Message{
		msg("system", tokContent(5)),
		msg("user", tokContent(5)),
		msg("assistant", tokContent(5)),
		msg("tool", tokContent(5)),
		msg("user", tokContent(10)),
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategyLastUser,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Messages) != 1 || res.Messages[0].Role != "user" || res.Messages[0].Content != msgs[4].Content {
		t.Errorf("unexpected result: %v", res.Messages)
	}
}

// --- Edge cases ---

func TestPlan_SingleSystemMessage(t *testing.T) {
	res, err := Plan(PlanInput{
		Messages:          []Message{msg("system", tokContent(10))},
		ModelContextLimit: 100,
		Strategy:          StrategySystemPlusLastUser,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No user message.
	if res.OverflowKind != OverflowAfterStrategy {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowAfterStrategy)
	}
}

func TestPlanRecentTurns_SystemExceedsBudget(t *testing.T) {
	// System messages alone exceed the budget — remaining clamps to 0,
	// so no body messages are kept.
	msgs := []Message{
		msg("system", tokContent(60)),
		msg("user", tokContent(10)),
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 50, // system alone is 60 tokens > 50
		Strategy:          StrategyRecentTurns,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// sysToks(60) > budget(50) → overflow.
	if res.OverflowKind != OverflowAfterStrategy {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowAfterStrategy)
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true")
	}
}

func TestPlanRecentTurns_ToolRoleMessage(t *testing.T) {
	// A tool message that does not form a user+assistant pair should be
	// treated as a single-message turn by planRecentTurns.
	msgs := []Message{
		msg("system", tokContent(5)),
		msg("user", tokContent(5)),
		msg("assistant", tokContent(5)),
		msg("tool", tokContent(5)), // unexpected: tool at tail
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 100,
		Strategy:          StrategyRecentTurns,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// All messages should fit within budget.
	if res.OverflowKind != OverflowNone {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowNone)
	}
}

func TestPlanHeadPlusTail_SystemExceedsBudget(t *testing.T) {
	// System messages alone exceed the total budget — bodyBudget goes negative.
	msgs := []Message{
		msg("system", tokContent(60)),
		msg("user", tokContent(10)),
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 50, // system alone is 60 tokens > 50
		Strategy:          StrategyHeadPlusTail,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// With bodyBudget clamped to 0, head and tail are both empty.
	// System is still returned; total > budget → OverflowAfterStrategy.
	if res.OverflowKind != OverflowAfterStrategy {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowAfterStrategy)
	}
}

func TestPlan_AllStrategies_EmptyMessages(t *testing.T) {
	strategies := []Strategy{
		StrategyLastUser,
		StrategySystemPlusLastUser,
		StrategyRecentTurns,
		StrategyHeadPlusTail,
		StrategyFullTruncated,
	}
	for _, s := range strategies {
		t.Run(string(s), func(t *testing.T) {
			res, err := Plan(PlanInput{
				Messages:          []Message{},
				ModelContextLimit: 100,
				Strategy:          s,
			})
			if err != nil {
				t.Fatalf("unexpected error for strategy %q: %v", s, err)
			}
			_ = res // just verify no panic
		})
	}
}
