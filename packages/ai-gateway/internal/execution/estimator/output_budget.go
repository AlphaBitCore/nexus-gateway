package estimator

// outputBudgetTable maps (model code, reasoning_effort) to the expected
// output token count anchor. The expandRange function widens this anchor
// into a low/expected/high envelope.
//
// Sources of the figures (anchor values; expandRange widens):
//   - OpenAI gpt-5/o-series:   https://platform.openai.com/docs/guides/reasoning (Sec. "Reasoning tokens")
//   - Anthropic extended thinking: https://docs.anthropic.com/en/docs/build-with-claude/extended-thinking
//   - Gemini 2.5 thinking:     https://ai.google.dev/gemini-api/docs/thinking
//
// Models not in this table return (0, false) and the caller falls back
// to a generic budget anchored at max_tokens/4.
//
// Maintenance: refreshing the figures is a data-only edit; no code
// changes required. A future calibration script can produce a fresh
// table from real-traffic samples.
var outputBudgetTable = map[string]map[string]int{
	// OpenAI reasoning models.
	"gpt-5":      {"minimal": 200, "low": 500, "medium": 1500, "high": 5000},
	"gpt-5-mini": {"minimal": 150, "low": 400, "medium": 1200, "high": 4000},
	"o3":         {"minimal": 200, "low": 600, "medium": 2000, "high": 8000},
	"o3-mini":    {"minimal": 150, "low": 400, "medium": 1200, "high": 4000},
	"o4-mini":    {"minimal": 150, "low": 500, "medium": 1500, "high": 6000},

	// Anthropic extended-thinking models (effort buckets approximated
	// from budget_tokens ranges: low ≈ 2k, medium ≈ 8k, high ≈ 24k).
	"claude-opus-4-7":   {"low": 800, "medium": 4000, "high": 12000},
	"claude-opus-4-6":   {"low": 800, "medium": 4000, "high": 12000},
	"claude-sonnet-4-7": {"low": 600, "medium": 3000, "high": 10000},
	"claude-sonnet-4-6": {"low": 600, "medium": 3000, "high": 10000},
	"claude-haiku-4-5":  {"low": 400, "medium": 1500, "high": 5000},

	// Gemini 2.5 thinking models.
	"gemini-2.5-pro":   {"low": 600, "medium": 3000, "high": 10000},
	"gemini-2.5-flash": {"low": 400, "medium": 1500, "high": 5000},

	// DeepSeek V4 reasoner (reasoning_effort high/max; reasoning_content
	// returned on the OpenAI-compatible wire). Anchors approximated from
	// the comparable reasoner tier pending a real-traffic calibration pass.
	"deepseek-v4-pro":   {"low": 600, "medium": 2500, "high": 8000},
	"deepseek-v4-flash": {"low": 300, "medium": 1200, "high": 4000},

	// Moonshot/Kimi thinking models (thinking is a mode of k2.5/k2.6;
	// effort is not graduated upstream, so effort="" falls to the "low"
	// anchor). Approximated pending calibration.
	"kimi-k2.6": {"low": 600, "medium": 3000, "high": 10000},
	"kimi-k2.5": {"low": 500, "medium": 2500, "high": 8000},
}

// lookupOutputBudget returns (anchor, supports). When the model is not
// in the table, returns (0, false) so the caller can pick a generic
// fallback. When the model IS in the table but the effort key is not
// recognised, returns (0, true) — the model supports reasoning, the
// caller just didn't request a valid effort.
func lookupOutputBudget(modelCode, effort string) (int, bool) {
	tbl, ok := outputBudgetTable[modelCode]
	if !ok {
		return 0, false
	}
	if effort == "" {
		// Model supports reasoning but caller didn't request it —
		// return the "low" anchor as a sane default; supports=true.
		if v, ok := tbl["low"]; ok {
			return v, true
		}
		return 0, true
	}
	v, ok := tbl[effort]
	if !ok {
		return 0, true
	}
	return v, true
}
