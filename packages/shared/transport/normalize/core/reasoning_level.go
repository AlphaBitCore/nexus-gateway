package core

// EffortForBudget answers "what effort level is this many reasoning tokens",
// in the canonical vocabulary.
//
// Two places need the answer and they must not disagree. Canonicalization asks
// it so a caller who sized their reasoning in TOKENS still says something a
// level-taking wire can read; cost estimation asks it so the output anchor
// matches the level the request will actually carry. If those two bucketed
// differently, a request would be priced against one level and sent at another.
//
// The boundaries are the vocabulary's, not any provider's: this maps a
// quantity onto four words, and every wire that takes an amount gets the exact
// figure from its own extension rather than from this. A budget of zero or less
// is not a small amount of reasoning — it is the absence of a quantity, and the
// caller's "do not reason" is spelled by the level "none", not by a number.
func EffortForBudget(budget int) string {
	switch {
	case budget <= 0:
		return ""
	case budget < 2000:
		return "low"
	case budget < 8000:
		return "medium"
	default:
		return "high"
	}
}
