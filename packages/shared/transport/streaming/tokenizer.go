package streaming

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"
)

// Tokenizer estimates the token count of a text string. Implementations are
// not required to be deterministic across provider versions — only stable
// enough for usage attribution/billing estimates (documented accuracy band).
type Tokenizer interface {
	// Count returns the estimated token count for text. Should respect
	// ctx.Deadline for bounded runtime; callers wrap this in
	// countWithDeadline for an explicit budget.
	Count(ctx context.Context, text string) (int, error)
}

// ErrTokenizerTimeout indicates the tokenizer did not produce a count within
// the caller's deadline.
var ErrTokenizerTimeout = errors.New("tokenizer: deadline exceeded")

// tokenizerDeadline bounds each tokenizer invocation. Per the SDD this keeps
// Tier-2 estimation off the hot path when the text is pathological
// (e.g. multi-MB completions).
const tokenizerDeadline = 200 * time.Millisecond

// countWithDeadline runs tok.Count with a bounded deadline derived from the
// caller's ctx. Returns ErrTokenizerTimeout on deadline expiry.
func countWithDeadline(parent context.Context, tok Tokenizer, text string) (int, error) {
	if tok == nil {
		return 0, errors.New("tokenizer: nil tokenizer")
	}
	if text == "" {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(parent, tokenizerDeadline)
	defer cancel()

	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := tok.Count(ctx, text)
		ch <- result{n: n, err: err}
	}()
	select {
	case r := <-ch:
		return r.n, r.err
	case <-ctx.Done():
		return 0, ErrTokenizerTimeout
	}
}

// tokenizerFor returns the Tokenizer appropriate for a provider. All built-in
// tokenizers currently use the heuristic estimator (characters/4); real
// tiktoken / anthropic-tokenizer / SentencePiece backends can swap in here
// without changing caller code. The heuristic has a documented ±15% accuracy
// band for English text, wider for CJK (tokenizer_test covers this).
func tokenizerFor(providerID string) Tokenizer {
	switch providerID {
	case "openai", "azure", "deepseek", "glm", "minimax":
		return heuristicGPT{}
	case "anthropic":
		return heuristicAnthropic{}
	case "gemini":
		return heuristicSentencePiece{}
	}
	return heuristicGPT{}
}

// heuristicGPT estimates GPT-shape tokenization as ceil(chars / 4). OpenAI
// documents this as the canonical rough estimate for English text. Accurate
// to ~±15% for English prose; less accurate for code or CJK.
type heuristicGPT struct{}

func (heuristicGPT) Count(_ context.Context, text string) (int, error) {
	if text == "" {
		return 0, nil
	}
	// RuneCount handles multi-byte UTF-8 (CJK, emoji) so we don't
	// over-count bytes.
	runes := utf8.RuneCountInString(text)
	n := runes / 4
	if runes%4 != 0 {
		n++
	}
	if n < 1 {
		n = 1
	}
	return n, nil
}

// heuristicAnthropic — Claude's tokenizer is ~20% denser than GPT on
// English prose (per Anthropic's own benchmarks), i.e. each token covers
// more characters, yielding fewer tokens for the same text.
type heuristicAnthropic struct{}

func (heuristicAnthropic) Count(_ context.Context, text string) (int, error) {
	if text == "" {
		return 0, nil
	}
	runes := utf8.RuneCountInString(text)
	n := runes / 5 // ≈ 20% fewer tokens than GPT's chars/4
	if runes%5 != 0 {
		n++
	}
	if n < 1 {
		n = 1
	}
	return n, nil
}

// heuristicSentencePiece — Gemini uses SentencePiece (similar density to
// GPT for English). Documented ±15% accuracy band.
type heuristicSentencePiece struct{}

func (heuristicSentencePiece) Count(_ context.Context, text string) (int, error) {
	if text == "" {
		return 0, nil
	}
	runes := utf8.RuneCountInString(text)
	n := runes / 4
	if runes%4 != 0 {
		n++
	}
	if n < 1 {
		n = 1
	}
	return n, nil
}
