package inputstaging

import "errors"

// Strategy controls how [Plan] selects and orders messages when the
// conversation exceeds the available token budget.
type Strategy string

const (
	// StrategyLastUser keeps only the latest message with Role="user".
	// Useful for very small context windows where only the immediate
	// question matters.
	StrategyLastUser Strategy = "last_user"

	// StrategySystemPlusLastUser keeps ALL system messages (in order) plus
	// the latest user message.  This is the default strategy and balances
	// instruction fidelity with context efficiency.
	StrategySystemPlusLastUser Strategy = "system_plus_last_user"

	// StrategyRecentTurns keeps system messages and then the most recent
	// user/assistant pairs that fit within the budget.  The number of
	// turns (K) is maximised subject to the token limit.
	StrategyRecentTurns Strategy = "recent_turns"

	// StrategyHeadPlusTail keeps system messages, the first user/assistant
	// exchange, and the last user/assistant exchange.  Preserves context
	// anchors at both ends of long conversations.
	StrategyHeadPlusTail Strategy = "head_plus_tail"

	// StrategyFullTruncated attempts to keep all messages in order; if the
	// total exceeds the budget it returns OverflowAfterStrategy and sets
	// Truncated=true without removing content — the caller decides what
	// to do next.
	StrategyFullTruncated Strategy = "full_truncated"
)

// Valid reports whether s is one of the five defined strategy values.
func (s Strategy) Valid() bool {
	switch s {
	case StrategyLastUser,
		StrategySystemPlusLastUser,
		StrategyRecentTurns,
		StrategyHeadPlusTail,
		StrategyFullTruncated:
		return true
	}
	return false
}

// Profile is a hint about the expected output length of the model's
// response.  [Suggest] uses it together with the model's context limit to
// recommend an appropriate strategy.
type Profile string

const (
	// ProfileGeneric is the default profile for mixed or unknown workloads.
	ProfileGeneric Profile = "generic"

	// ProfileShortAnswer describes workloads where the model's completion
	// is expected to be short (typically ≤256 tokens): Q&A, classification,
	// summarisation with a fixed template.
	ProfileShortAnswer Profile = "short_answer"

	// ProfileLongCompletion describes workloads where the model produces a
	// large response (typically ~2 k tokens): code generation, long-form
	// writing, detailed analysis.
	ProfileLongCompletion Profile = "long_completion"
)

// OverflowKind describes what happened when the selected strategy could not
// fit all desired content within the token budget.
type OverflowKind string

const (
	// OverflowNone indicates that the returned messages fit within the
	// budget after the strategy was applied.  Truncated may still be true
	// if the strategy dropped messages that were within budget.
	OverflowNone OverflowKind = ""

	// OverflowSingleMessageTooBig indicates that the single message the
	// strategy wanted to keep (typically the user query) is itself larger
	// than the entire available budget.  The caller should skip embedding
	// or fall back to a larger-context model.
	OverflowSingleMessageTooBig OverflowKind = "single_message_too_big"

	// OverflowAfterStrategy indicates that even after the strategy was
	// applied the remaining messages exceed the budget.  Messages in
	// PlanResult contains the strategy's output unchanged; the caller
	// decides whether to use it anyway or take another action.
	OverflowAfterStrategy OverflowKind = "after_strategy"
)

// ErrInvalidStrategy is returned by [Plan] when PlanInput.Strategy is not
// one of the five defined values.
var ErrInvalidStrategy = errors.New("inputstaging: invalid strategy")
