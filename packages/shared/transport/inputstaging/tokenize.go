package inputstaging

import (
	"unicode"
	"unicode/utf8"
)

// EstimateTokens returns an approximate token count for text using a
// fast character-based heuristic.  The approximation is intentionally
// coarse — it is not designed to match any specific tokeniser (BPE,
// SentencePiece, etc.).  Its only purpose is to decide whether a
// conversation fits within a model's context window before sending it to
// an embedding provider; the embedding provider will compute the real
// token count.
//
// # Heuristic
//
// For each Unicode rune:
//   - ASCII letters/digits/punctuation (code point < 128): counted as
//     0.25 tokens each, so 4 ASCII characters ≈ 1 token.  This matches
//     the widely-cited "1 token ≈ 4 English characters" rule of thumb
//     from OpenAI's documentation.
//   - CJK Unified Ideographs and other wide scripts (code point ≥ 0x2E80):
//     counted as 0.5 tokens each, so 2 CJK characters ≈ 1 token.  CJK
//     characters are individually meaningful (morpheme-level) and modern
//     BPE tokenisers typically allocate one or two tokens per character,
//     making 0.5 a reasonable midpoint.
//   - Other Unicode (diacritics, symbols, etc.): counted as 0.4 tokens,
//     between the ASCII and CJK rates.
//
// The result is rounded up so that a non-empty string never returns zero.
//
// Future epics that require precision (e.g. tiktoken-compatible counts)
// can swap this implementation without changing the [Plan] / [Suggest]
// API surface.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	var score float64
	for _, r := range text {
		score += runeTokenScore(r)
	}
	// Round up: a non-empty string always costs at least 1 token.
	n := int(score)
	if score > float64(n) {
		n++
	}
	return n
}

// runeTokenScore returns the heuristic token weight of a single rune. Shared by
// EstimateTokens and TruncateToTokens so the cut and the count agree exactly.
func runeTokenScore(r rune) float64 {
	switch {
	case r < 128:
		// ASCII path — 0.25 tokens per character (≈4 chars/token).
		return 0.25
	case isCJKLike(r):
		// Wide-script path — 0.5 tokens per character (≈2 chars/token).
		return 0.5
	default:
		// Other Unicode (diacritics, symbols, emoji, etc.) — 0.4 tokens.
		return 0.4
	}
}

// TruncateToTokens returns the longest TRAILING suffix of text whose estimated
// token count stays within maxTokens, applying a safety margin. It is the
// last-resort hard cut for callers (L2 embedding input, ai-guard classify
// input) that join inputstaging.Plan output into a single string: Plan drops
// whole messages but never cuts WITHIN one, so a single oversized message would
// otherwise be sent to the model over-limit and 400.
//
// It keeps the TAIL, not the head: the newest content (the latest user turn)
// sits at the end of the joined input and is what the embedding / classifier
// must reflect — dropping recent content to preserve an old system preamble
// would defeat the purpose. Oldest content (head) is discarded first.
//
// Because EstimateTokens is coarse and can UNDER-count dense content, the cut
// targets ~85% of maxTokens so the provider's real tokenizer is very unlikely
// to exceed the true limit. Returns text unchanged when it already fits, when
// maxTokens <= 0, or when text is empty. The returned suffix always lands on a
// UTF-8 rune boundary.
func TruncateToTokens(text string, maxTokens int) string {
	if maxTokens <= 0 || text == "" {
		return text
	}
	if EstimateTokens(text) <= maxTokens {
		return text
	}
	target := float64(maxTokens) * 0.85
	var score float64
	offset := len(text) // start byte of the kept suffix; len means "nothing yet"
	for offset > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:offset])
		if score+runeTokenScore(r) > target {
			break // including this older rune would exceed the budget — stop
		}
		score += runeTokenScore(r)
		offset -= size
	}
	return text[offset:]
}

// isCJKLike reports whether r falls in a script range that tokenises at
// roughly the morpheme level (one tokeniser token per 1-2 characters).
// Covers CJK Unified Ideographs and related blocks, Hangul, Hiragana,
// Katakana, and several CJK extension planes.  The threshold 0x2E80
// (start of the CJK Radicals Supplement block) is a practical lower
// bound that captures the bulk of East Asian text while excluding Latin
// extended and IPA characters that sit below it.
func isCJKLike(r rune) bool {
	// Use the unicode package to avoid hard-coded range tables that would
	// need maintenance as Unicode versions expand.
	return unicode.In(r,
		unicode.Han,
		unicode.Hangul,
		unicode.Hiragana,
		unicode.Katakana,
		unicode.Bopomofo,
	)
}
