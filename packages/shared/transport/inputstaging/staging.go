package inputstaging

import "fmt"

// Message is a single turn in a multi-turn conversation.
// Content is the plain text of the message; callers that hold structured
// messages (e.g. []ContentBlock) must join all text blocks into a single
// string before passing to [Plan].  This keeps the package free of
// upstream-type dependencies.
type Message struct {
	// Role is one of "system", "user", "assistant", or "tool".
	Role string
	// Content is the full text of the message.
	Content string
}

// PlanInput groups all inputs for a [Plan] call.
type PlanInput struct {
	// Messages is the full conversation, ordered chronologically.
	Messages []Message

	// ModelContextLimit is the total token capacity of the target model.
	// Must be >= 1.
	ModelContextLimit int

	// Strategy controls which messages are kept when the conversation
	// exceeds the available budget.
	Strategy Strategy

	// ReserveOutput is the number of tokens to reserve for the model's
	// completion response.  The effective input budget is
	// max(ModelContextLimit - ReserveOutput, 0).  May be 0.
	ReserveOutput int
}

// PlanResult is the output of [Plan].
type PlanResult struct {
	// Messages is the subset of the input conversation that fits within
	// the token budget after the strategy was applied.
	Messages []Message

	// InputTokens is the estimated token count for the returned Messages.
	InputTokens int

	// Truncated is true when Plan dropped at least one input message,
	// regardless of whether an overflow occurred.
	Truncated bool

	// OverflowKind describes the nature of the overflow, if any.
	// OverflowNone ("") means the result fits within the budget.
	OverflowKind OverflowKind
}

// Plan selects and orders messages from in.Messages so that the estimated
// token count fits within (in.ModelContextLimit - in.ReserveOutput).
//
// The strategy governs which messages are kept:
//   - [StrategyLastUser]: only the latest "user" message.
//   - [StrategySystemPlusLastUser]: all "system" messages + latest "user".
//   - [StrategyRecentTurns]: system + as many trailing user/assistant pairs as fit.
//   - [StrategyHeadPlusTail]: system + first exchange + last exchange.
//   - [StrategyFullTruncated]: all messages; overflow is reported but content is not cut.
//
// Plan returns [ErrInvalidStrategy] when in.Strategy is not one of the
// five defined values.  All other errors are formatting errors on the
// input struct.
func Plan(in PlanInput) (PlanResult, error) {
	if !in.Strategy.Valid() {
		return PlanResult{}, fmt.Errorf("%w: %q", ErrInvalidStrategy, in.Strategy)
	}
	if in.ModelContextLimit < 1 {
		return PlanResult{}, fmt.Errorf("inputstaging: ModelContextLimit must be >= 1, got %d", in.ModelContextLimit)
	}

	budget := in.ModelContextLimit - in.ReserveOutput
	if budget < 0 {
		budget = 0
	}

	switch in.Strategy {
	case StrategyLastUser:
		return planLastUser(in.Messages, budget)
	case StrategySystemPlusLastUser:
		return planSystemPlusLastUser(in.Messages, budget)
	case StrategyRecentTurns:
		return planRecentTurns(in.Messages, budget)
	case StrategyHeadPlusTail:
		return planHeadPlusTail(in.Messages, budget)
	default: // StrategyFullTruncated — the only remaining valid value after Valid() above
		return planFullTruncated(in.Messages, budget)
	}
}

// Suggest recommends a [Strategy] based on the model's context size and
// the expected output profile of the workload.
//
// The recommendation is a heuristic: it is safe to override via admin
// config.  The admin UI's input-staging selector calls this to highlight
// the recommended option while still allowing admin override.
//
// Heuristic table:
//
//	context_limit <= 1024               → last_user         (tiny window)
//	1024 < limit <= 4096, generic       → system_plus_last_user
//	1024 < limit <= 4096, long_compl.   → last_user          (output eats budget)
//	4096 < limit <= 16384, generic/short→ system_plus_last_user
//	4096 < limit <= 16384, long_compl.  → recent_turns
//	limit > 16384                        → recent_turns      (large window, use context)
func Suggest(modelContextLimit int, profile Profile) Strategy {
	switch {
	case modelContextLimit <= 1024:
		return StrategyLastUser
	case modelContextLimit <= 4096:
		if profile == ProfileLongCompletion {
			return StrategyLastUser
		}
		return StrategySystemPlusLastUser
	case modelContextLimit <= 16384:
		if profile == ProfileLongCompletion {
			return StrategyRecentTurns
		}
		return StrategySystemPlusLastUser
	default:
		// modelContextLimit > 16384
		return StrategyRecentTurns
	}
}

// --- strategy implementations ---

// planLastUser keeps only the last message with role="user".
func planLastUser(msgs []Message, budget int) (PlanResult, error) {
	var lastUser *Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			lastUser = &msgs[i]
			break
		}
	}
	if lastUser == nil {
		// No user message in the conversation.
		return PlanResult{
			Messages:     []Message{},
			OverflowKind: OverflowAfterStrategy,
			Truncated:    len(msgs) > 0,
		}, nil
	}

	toks := EstimateTokens(lastUser.Content)
	truncated := len(msgs) > 1 || msgs[len(msgs)-1].Role != "user"

	if toks > budget {
		return PlanResult{
			Messages:     []Message{*lastUser},
			InputTokens:  toks,
			Truncated:    truncated,
			OverflowKind: OverflowSingleMessageTooBig,
		}, nil
	}

	return PlanResult{
		Messages:     []Message{*lastUser},
		InputTokens:  toks,
		Truncated:    truncated,
		OverflowKind: OverflowNone,
	}, nil
}

// planSystemPlusLastUser keeps all system messages (in order) plus the
// last user message.
func planSystemPlusLastUser(msgs []Message, budget int) (PlanResult, error) {
	var systemMsgs []Message
	var lastUser *Message

	for i := range msgs {
		if msgs[i].Role == "system" {
			systemMsgs = append(systemMsgs, msgs[i])
		}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			lastUser = &msgs[i]
			break
		}
	}

	truncated := len(msgs) > (len(systemMsgs) + boolToInt(lastUser != nil))

	var selected []Message
	selected = append(selected, systemMsgs...)
	if lastUser != nil {
		selected = append(selected, *lastUser)
	}

	toks := estimateTokensForMessages(selected)

	if lastUser == nil {
		// No user message; the strategy cannot produce a useful result.
		return PlanResult{
			Messages:     selected,
			InputTokens:  toks,
			Truncated:    truncated,
			OverflowKind: OverflowAfterStrategy,
		}, nil
	}

	// Check if the single essential message (last user) alone exceeds budget.
	lastUserToks := EstimateTokens(lastUser.Content)
	if lastUserToks > budget {
		return PlanResult{
			Messages:     []Message{*lastUser},
			InputTokens:  lastUserToks,
			Truncated:    true,
			OverflowKind: OverflowSingleMessageTooBig,
		}, nil
	}

	if toks > budget {
		// System messages push total over budget; return just the user message.
		return PlanResult{
			Messages:     []Message{*lastUser},
			InputTokens:  lastUserToks,
			Truncated:    true,
			OverflowKind: OverflowAfterStrategy,
		}, nil
	}

	return PlanResult{
		Messages:     selected,
		InputTokens:  toks,
		Truncated:    truncated,
		OverflowKind: OverflowNone,
	}, nil
}

// planRecentTurns keeps all system messages and then fills the remaining
// budget with the most recent user/assistant turn pairs, working backwards
// from the end of the conversation.
//
// A "turn" is a user+assistant message pair.  An unpaired user message at
// the very end of the conversation counts as a single-message turn.  The
// algorithm adds complete turns (both messages in the pair) or the trailing
// single user message — it does not split a pair.  This matches the SDD
// definition: "stop when adding one more [turn] would exceed the budget".
func planRecentTurns(msgs []Message, budget int) (PlanResult, error) {
	// Separate system messages from the conversation body.
	var systemMsgs []Message
	var bodyMsgs []Message
	for i := range msgs {
		if msgs[i].Role == "system" {
			systemMsgs = append(systemMsgs, msgs[i])
		} else {
			bodyMsgs = append(bodyMsgs, msgs[i])
		}
	}

	sysToks := estimateTokensForMessages(systemMsgs)
	remaining := budget - sysToks
	if remaining < 0 {
		remaining = 0
	}

	// Group bodyMsgs into turns from the end.
	// A turn is a user+assistant pair or a trailing single user message.
	// We collect turn indices in reverse order then reverse at the end
	// to avoid O(n²) prepend allocations.
	type turnBounds struct{ lo, hi int } // inclusive indices into bodyMsgs
	var turns []turnBounds
	keptToks := 0
	i := len(bodyMsgs) - 1
	for i >= 0 {
		var lo, hi int
		switch {
		case bodyMsgs[i].Role == "user":
			// Trailing single user message.
			lo, hi = i, i
			i--
		case bodyMsgs[i].Role == "assistant" && i > 0 && bodyMsgs[i-1].Role == "user":
			// user + assistant pair.
			lo, hi = i-1, i
			i -= 2
		default:
			// Tool message or unexpected role order — treat as single-message turn.
			lo, hi = i, i
			i--
		}

		turnToks := 0
		for k := lo; k <= hi; k++ {
			turnToks += EstimateTokens(bodyMsgs[k].Content)
		}
		if keptToks+turnToks > remaining {
			break
		}
		keptToks += turnToks
		turns = append(turns, turnBounds{lo, hi})
	}

	// Reverse turns so they are in chronological (oldest-first) order.
	for a, b := 0, len(turns)-1; a < b; a, b = a+1, b-1 {
		turns[a], turns[b] = turns[b], turns[a]
	}

	// Build kept slice in one allocation.
	keptLen := 0
	for _, tb := range turns {
		keptLen += tb.hi - tb.lo + 1
	}
	kept := make([]Message, 0, keptLen)
	for _, tb := range turns {
		kept = append(kept, bodyMsgs[tb.lo:tb.hi+1]...)
	}

	truncated := len(msgs) > len(systemMsgs)+len(kept)
	selected := make([]Message, 0, len(systemMsgs)+len(kept))
	selected = append(selected, systemMsgs...)
	selected = append(selected, kept...)
	totalToks := sysToks + keptToks

	if totalToks > budget {
		return PlanResult{
			Messages:     selected,
			InputTokens:  totalToks,
			Truncated:    truncated,
			OverflowKind: OverflowAfterStrategy,
		}, nil
	}

	return PlanResult{
		Messages:     selected,
		InputTokens:  totalToks,
		Truncated:    truncated,
		OverflowKind: OverflowNone,
	}, nil
}

// planHeadPlusTail keeps system messages, the first non-system
// user/assistant exchange (head), and the last user/assistant exchange
// (tail).  Budget is split 30% head / 70% tail.  When the first and last
// exchanges are the same (short conversation), they are deduped.
func planHeadPlusTail(msgs []Message, budget int) (PlanResult, error) {
	var systemMsgs []Message
	var bodyMsgs []Message
	for i := range msgs {
		if msgs[i].Role == "system" {
			systemMsgs = append(systemMsgs, msgs[i])
		} else {
			bodyMsgs = append(bodyMsgs, msgs[i])
		}
	}

	sysToks := estimateTokensForMessages(systemMsgs)
	bodyBudget := budget - sysToks
	if bodyBudget < 0 {
		bodyBudget = 0
	}

	// 30% head, 70% tail.
	headBudget := bodyBudget * 30 / 100
	tailBudget := bodyBudget - headBudget

	// Collect head: first pair (or first single message) from bodyMsgs.
	var headMsgs []Message
	headToks := 0
	for i := 0; i < len(bodyMsgs) && i < 2; i++ {
		t := EstimateTokens(bodyMsgs[i].Content)
		if headToks+t > headBudget {
			break
		}
		headToks += t
		headMsgs = append(headMsgs, bodyMsgs[i])
	}

	// Collect tail: fill from the end of bodyMsgs, skipping indices already
	// in head.  Collect in reverse then reverse at the end to avoid O(n²)
	// prepend allocations.
	headLen := len(headMsgs)
	var tailIdxRev []int // body indices in reverse (end→start)
	tailToks := 0
	for i := len(bodyMsgs) - 1; i >= headLen; i-- {
		t := EstimateTokens(bodyMsgs[i].Content)
		if tailToks+t > tailBudget {
			break
		}
		tailToks += t
		tailIdxRev = append(tailIdxRev, i)
	}
	// Reverse to get chronological order and build tailMsgs in one alloc.
	tailMsgs := make([]Message, len(tailIdxRev))
	for j, idx := range tailIdxRev {
		tailMsgs[len(tailIdxRev)-1-j] = bodyMsgs[idx]
	}

	selected := make([]Message, 0, len(systemMsgs)+len(headMsgs)+len(tailMsgs))
	selected = append(selected, systemMsgs...)
	selected = append(selected, headMsgs...)
	selected = append(selected, tailMsgs...)
	totalToks := sysToks + headToks + tailToks
	truncated := len(msgs) > len(selected)

	if totalToks > budget {
		return PlanResult{
			Messages:     selected,
			InputTokens:  totalToks,
			Truncated:    truncated,
			OverflowKind: OverflowAfterStrategy,
		}, nil
	}

	return PlanResult{
		Messages:     selected,
		InputTokens:  totalToks,
		Truncated:    truncated,
		OverflowKind: OverflowNone,
	}, nil
}

// planFullTruncated keeps all messages in order.  If the total token
// count exceeds the budget, it sets OverflowAfterStrategy and Truncated=true
// without modifying message content — the caller decides what to do.
func planFullTruncated(msgs []Message, budget int) (PlanResult, error) {
	toks := estimateTokensForMessages(msgs)

	if toks > budget {
		// Check if even the last single message exceeds budget.
		if len(msgs) > 0 {
			lastToks := EstimateTokens(msgs[len(msgs)-1].Content)
			if lastToks > budget {
				return PlanResult{
					Messages:     msgs,
					InputTokens:  toks,
					Truncated:    true,
					OverflowKind: OverflowSingleMessageTooBig,
				}, nil
			}
		}
		return PlanResult{
			Messages:     msgs,
			InputTokens:  toks,
			Truncated:    true,
			OverflowKind: OverflowAfterStrategy,
		}, nil
	}

	return PlanResult{
		Messages:     msgs,
		InputTokens:  toks,
		Truncated:    false,
		OverflowKind: OverflowNone,
	}, nil
}

// --- helpers ---

// estimateTokensForMessages sums EstimateTokens over every message.
func estimateTokensForMessages(msgs []Message) int {
	total := 0
	for i := range msgs {
		total += EstimateTokens(msgs[i].Content)
	}
	return total
}

// boolToInt returns 1 if b is true, 0 otherwise.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
