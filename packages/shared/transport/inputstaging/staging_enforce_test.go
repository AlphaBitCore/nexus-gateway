package inputstaging

import (
	"strings"
	"testing"
)

// Budget-enforcement contract (the DEFAULT): Plan's returned Messages are
// guaranteed to fit the budget — oldest messages are dropped first, a
// single still-oversized message is tail-truncated, and a strategy that
// selected nothing from a non-empty conversation is re-seeded with the
// newest user content so the caller never sends an empty prompt.
// OverflowKind keeps reporting the strategy-stage condition.

// ASCII maths: strings.Repeat("a", 4*N) estimates to exactly N tokens.

func TestPlan_EnforceDefault_OversizedSingleMessageTruncated(t *testing.T) {
	huge := strings.Repeat("a", 4000) // 1000 tokens
	res, err := Plan(PlanInput{
		Messages:          []Message{{Role: "user", Content: huge}},
		ModelContextLimit: 100, // budget 100 - 0 reserve
		Strategy:          StrategyLastUser,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(res.Messages))
	}
	if got := EstimateTokens(res.Messages[0].Content); got > 100 {
		t.Errorf("enforced message estimates %d tokens, budget 100", got)
	}
	if res.Messages[0].Content == "" {
		t.Errorf("enforcement must not empty the message")
	}
	if !strings.HasSuffix(huge, res.Messages[0].Content) {
		t.Errorf("cut must keep the tail (newest content)")
	}
	if res.OverflowKind != OverflowSingleMessageTooBig {
		t.Errorf("OverflowKind must keep reporting the strategy-stage condition, got %q", res.OverflowKind)
	}
	if !res.Truncated {
		t.Errorf("Truncated must be true after enforcement")
	}
	if res.InputTokens > 100 {
		t.Errorf("InputTokens %d must reflect the returned (enforced) messages", res.InputTokens)
	}
}

func TestPlan_ReportOnly_KeepsDiagnoseOnlyOversizedBehavior(t *testing.T) {
	huge := strings.Repeat("a", 4000)
	res, err := Plan(PlanInput{
		Messages:          []Message{{Role: "user", Content: huge}},
		ModelContextLimit: 100,
		Strategy:          StrategyLastUser,
		ReportOnly:        true,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(res.Messages) != 1 || res.Messages[0].Content != huge {
		t.Fatalf("with ReportOnly the oversized message must pass through unchanged")
	}
	if res.OverflowKind != OverflowSingleMessageTooBig {
		t.Errorf("expected OverflowSingleMessageTooBig, got %q", res.OverflowKind)
	}
}

// A RecentTurns plan whose single trailing user turn exceeds the budget
// selects zero body messages today; by default the newest user
// content must be re-seeded and truncated rather than returned empty.
func TestPlan_EnforceDefault_RecentTurnsOversizedTurn_ReseedsLastUser(t *testing.T) {
	huge := strings.Repeat("a", 4000)
	res, err := Plan(PlanInput{
		Messages: []Message{
			{Role: "user", Content: "earlier question"},
			{Role: "assistant", Content: "earlier answer"},
			{Role: "user", Content: huge},
		},
		ModelContextLimit: 100,
		Strategy:          StrategyRecentTurns,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(res.Messages) == 0 {
		t.Fatalf("default enforcement must never return empty messages for a non-empty conversation")
	}
	total := 0
	for _, m := range res.Messages {
		total += EstimateTokens(m.Content)
	}
	if total > 100 {
		t.Errorf("returned messages estimate %d tokens, budget 100", total)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Role != "user" || !strings.HasSuffix(huge, last.Content) {
		t.Errorf("re-seeded content must be the tail of the newest user message")
	}
}

// Multi-message result over budget (FullTruncated reports overflow but
// cuts nothing): enforcement drops oldest messages first, newest wins.
func TestPlan_EnforceDefault_FullTruncated_DropsOldestFirst(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: strings.Repeat("a", 400)}, // 100 tokens (oldest)
		{Role: "assistant", Content: strings.Repeat("a", 400)},
		{Role: "user", Content: strings.Repeat("a", 200)}, // 50 tokens (newest)
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 130,
		Strategy:          StrategyFullTruncated,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(res.Messages) == 0 {
		t.Fatalf("expected messages to survive")
	}
	total := 0
	for _, m := range res.Messages {
		total += EstimateTokens(m.Content)
	}
	if total > 130 {
		t.Errorf("returned messages estimate %d tokens, budget 130", total)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Content != msgs[2].Content {
		t.Errorf("newest message must survive enforcement")
	}
	if !res.Truncated {
		t.Errorf("Truncated must be true")
	}
}

func TestPlan_EnforceDefault_FitsWithinBudget_NoOp(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "short question"},
	}
	res, err := Plan(PlanInput{
		Messages:          msgs,
		ModelContextLimit: 1000,
		Strategy:          StrategyLastUser,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(res.Messages) != 1 || res.Messages[0].Content != "short question" {
		t.Fatalf("in-budget plan must be untouched: %+v", res.Messages)
	}
	if res.OverflowKind != OverflowNone || res.Truncated {
		t.Errorf("no overflow expected: kind=%q truncated=%v", res.OverflowKind, res.Truncated)
	}
}

// Zero effective budget (reserve >= limit) is a misconfiguration under
// which the fit guarantee is unsatisfiable: enforcement fails open and
// returns the strategy result unchanged — callers must always receive
// content to forward (ai-guard's fail-open contract), and their
// upstream-error fallbacks own this corner.
func TestPlan_EnforceDefault_ZeroBudget_FailsOpenUnchanged(t *testing.T) {
	res, err := Plan(PlanInput{
		Messages:          []Message{{Role: "user", Content: "anything"}},
		ModelContextLimit: 10,
		ReserveOutput:     10,
		Strategy:          StrategyLastUser,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(res.Messages) != 1 || res.Messages[0].Content != "anything" {
		t.Errorf("zero budget must fail open with the strategy result unchanged, got %+v", res.Messages)
	}
	if res.OverflowKind != OverflowSingleMessageTooBig {
		t.Errorf("overflow must still be reported, got %q", res.OverflowKind)
	}
}

// TestPlanRecentTurns_NoTurnFits_ReportsOverflowAfterStrategy: a
// non-empty conversation body from which no turn fits must report
// OverflowAfterStrategy (consistent with the other strategies), not
// OverflowNone — callers key overflow logging and metrics off the kind,
// and a silently-empty selection is precisely the bounded case they
// need to observe. ReportOnly isolates the strategy-stage verdict.
func TestPlanRecentTurns_NoTurnFits_ReportsOverflowAfterStrategy(t *testing.T) {
	res, err := Plan(PlanInput{
		Messages:          []Message{{Role: "user", Content: strings.Repeat("a", 4000)}}, // ≈1000 tokens
		ModelContextLimit: 100,
		Strategy:          StrategyRecentTurns,
		ReportOnly:        true,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if res.OverflowKind != OverflowAfterStrategy {
		t.Errorf("OverflowKind = %q, want %q", res.OverflowKind, OverflowAfterStrategy)
	}
	if len(res.Messages) != 0 {
		t.Errorf("strategy-stage selection should be empty, got %+v", res.Messages)
	}
}
