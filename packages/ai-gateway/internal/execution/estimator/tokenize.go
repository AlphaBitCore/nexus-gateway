package estimator

import (
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// Tokenizer estimates the token count of an input character count. Per-
// family divisors approximate the average characters-per-token for that
// vendor's tokenizer. The contract is: pure function, deterministic,
// no I/O.
//
// This is the ACCURACY-class estimate: cost estimation wants the best
// average-case guess (per-family divisor), NOT an upper bound. Fit / "will
// it fit the context window?" decisions must instead use
// inputstaging.EstimateTokensConservative, which is biased high so it never
// admits a model the request overflows. Do not route fit checks here, and do
// not add a third char→token heuristic — pick the class that matches intent.
type Tokenizer interface {
	CountTokens(chars int) int
	IsHeuristic() bool
	Divisor() float64
}

// heuristicTokenizer divides chars by a per-family constant. Documented
// as ±10–15% typical error in EstimateResult.Assumptions[].
type heuristicTokenizer struct {
	divisor float64
}

func (h heuristicTokenizer) CountTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	t := int(float64(chars) / h.divisor)
	if t < 1 {
		return 1
	}
	return t
}

func (heuristicTokenizer) IsHeuristic() bool  { return true }
func (h heuristicTokenizer) Divisor() float64 { return h.divisor }

// pickTokenizer returns the right heuristic for the adapter family.
// Future: drop in tiktoken-go (OpenAI / Azure / OpenAI-compat) without
// changing this entry point — the Tokenizer interface is stable.
func pickTokenizer(adapterType string) Tokenizer {
	switch provcore.Format(adapterType) {
	case provcore.FormatGemini, provcore.FormatVertex:
		// Gemini's SentencePiece tends to coalesce slightly more
		// aggressively than tiktoken; chars/4 is a closer match.
		return heuristicTokenizer{divisor: 4.0}
	default:
		// OpenAI/Azure/Anthropic/DeepSeek/Moonshot/etc all sit
		// around 3.5 chars/token for English; multibyte text shifts
		// this materially but is the same drift for all of them.
		return heuristicTokenizer{divisor: 3.5}
	}
}
