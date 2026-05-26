package inputstaging

import (
	"fmt"
	"testing"
)

// buildConversation builds a synthetic 100-message conversation for benchmarks.
// It alternates user/assistant messages with a system message at index 0.
// Each message carries ~50 tokens of ASCII content.
func buildConversation(n int) []Message {
	msgs := make([]Message, 0, n+1)
	msgs = append(msgs, msg("system", tokContent(20)))
	for range n / 2 {
		msgs = append(msgs, msg("user", tokContent(50)))
		msgs = append(msgs, msg("assistant", tokContent(50)))
	}
	return msgs
}

// BenchmarkPlan_LastUser measures Plan throughput for StrategyLastUser on a
// 100-message conversation.  Target: <500µs/op on a development laptop.
func BenchmarkPlan_LastUser(b *testing.B) {
	msgs := buildConversation(100)
	in := PlanInput{
		Messages:          msgs,
		ModelContextLimit: 8192,
		Strategy:          StrategyLastUser,
		ReserveOutput:     512,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = Plan(in)
	}
}

// BenchmarkPlan_SystemPlusLastUser measures throughput for the default strategy.
func BenchmarkPlan_SystemPlusLastUser(b *testing.B) {
	msgs := buildConversation(100)
	in := PlanInput{
		Messages:          msgs,
		ModelContextLimit: 8192,
		Strategy:          StrategySystemPlusLastUser,
		ReserveOutput:     512,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = Plan(in)
	}
}

// BenchmarkPlan_RecentTurns is the most expensive strategy (walks the slice
// from the end); still expected to complete well within the 500µs budget.
func BenchmarkPlan_RecentTurns(b *testing.B) {
	msgs := buildConversation(100)
	in := PlanInput{
		Messages:          msgs,
		ModelContextLimit: 8192,
		Strategy:          StrategyRecentTurns,
		ReserveOutput:     512,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = Plan(in)
	}
}

// BenchmarkPlan_HeadPlusTail measures the head+tail strategy.
func BenchmarkPlan_HeadPlusTail(b *testing.B) {
	msgs := buildConversation(100)
	in := PlanInput{
		Messages:          msgs,
		ModelContextLimit: 8192,
		Strategy:          StrategyHeadPlusTail,
		ReserveOutput:     512,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = Plan(in)
	}
}

// BenchmarkPlan_FullTruncated measures the full-truncated strategy.
func BenchmarkPlan_FullTruncated(b *testing.B) {
	msgs := buildConversation(100)
	in := PlanInput{
		Messages:          msgs,
		ModelContextLimit: 8192,
		Strategy:          StrategyFullTruncated,
		ReserveOutput:     512,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = Plan(in)
	}
}

// BenchmarkEstimateTokens measures token estimation over a realistic message body.
func BenchmarkEstimateTokens(b *testing.B) {
	text := fmt.Sprintf("%s %s", tokContent(100), "world")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = EstimateTokens(text)
	}
}

// BenchmarkSuggest measures the Suggest function (pure table lookup — should
// be near zero ns/op).
func BenchmarkSuggest(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = Suggest(8192, ProfileGeneric)
	}
}
