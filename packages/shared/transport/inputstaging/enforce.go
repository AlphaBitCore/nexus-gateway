package inputstaging

// enforceBudget post-processes a strategy result so the returned
// Messages provably fit the budget. See PlanInput.ReportOnly for the
// contract (enforcement is the default; ReportOnly opts out). OverflowKind is preserved from the strategy stage;
// InputTokens always reflects the returned messages.
func enforceBudget(res PlanResult, original []Message, budget int) PlanResult {
	// A non-positive budget (ReserveOutput >= ModelContextLimit) is a
	// misconfiguration under which the fit guarantee is unsatisfiable.
	// Fail open with the strategy result unchanged — callers' existing
	// upstream-error fallbacks own this corner; returning nothing would
	// break fail-open contracts (ai-guard must always forward content).
	if budget <= 0 {
		return res
	}

	// Fast path: OverflowNone means the strategy already verified the
	// selection fits, and a non-empty selection needs no re-seeding.
	// This keeps enforcement at one comparison on the common path —
	// ai-guard runs it per classify call.
	if res.OverflowKind == OverflowNone && len(res.Messages) > 0 {
		return res
	}

	msgs := res.Messages
	changed := false

	// Re-seed: a strategy that selected nothing from a non-empty
	// conversation yields the newest user message (fallback: the newest
	// message of any role) so the caller never sends an empty prompt.
	reseeded := false
	if len(msgs) == 0 && len(original) > 0 {
		seed := original[len(original)-1]
		for i := len(original) - 1; i >= 0; i-- {
			if original[i].Role == "user" {
				seed = original[i]
				break
			}
		}
		msgs = []Message{seed}
		changed = true
		reseeded = true
	}

	// Every strategy returns InputTokens matching its Messages, so the
	// estimate is only recomputed after a re-seed replaced the selection.
	total := res.InputTokens
	if reseeded {
		total = estimateTokensForMessages(msgs)
	}

	// Drop oldest whole messages until the remainder fits.
	for len(msgs) > 1 && total > budget {
		total -= EstimateTokensConservative(msgs[0].Content)
		msgs = msgs[1:]
		changed = true
	}

	// A single message still over budget is tail-truncated in place.
	if len(msgs) == 1 && total > budget {
		cut := TruncateToTokens(msgs[0].Content, budget)
		msgs = []Message{{Role: msgs[0].Role, Content: cut}}
		total = EstimateTokensConservative(cut)
		changed = true
	}

	if changed {
		res.Messages = msgs
		res.InputTokens = total
		res.Truncated = true
	}
	return res
}
