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

// runeTokenScore returns the average-case heuristic token weight of a single
// rune, backing EstimateTokens. Fit decisions (routing, staging, truncation)
// use runeTokenScoreConservative instead.
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

// EstimateTokensConservative returns a deliberately HIGH token estimate,
// biased so it lands at or above what a real tokenizer charges for realistic
// text. It is the single entry point every FIT / SAFETY decision must use:
// routing's context-window filter, the router LLM's own input budget, the
// AI-Guard context limit, and the message-drop staging that keeps embedding
// input under a provider's cap. For those callers the asymmetry is total —
// under-counting selects a model (or keeps content) that then overflows and
// hard-400s upstream, while over-counting only selects a larger-context model
// (or trims a little early), which always succeeds. So the estimate is biased
// up, never down.
//
// Precisely: the non-ASCII weight (UTF-8 byte length) is a HARD ceiling — a
// BPE tokenizer emits at most one token per byte, so no input can exceed it.
// The ASCII weight (0.5/char) is NOT a hard ceiling — a byte-level fallback
// could in principle charge up to 1 token per ASCII byte — but 0.5 is double
// the "4 chars ≈ 1 token" prose average and covers dense code / minified JSON
// / base64 (~0.3–0.4/char) with headroom; the pathological remainder (near-1
// tok/char single-character-delimited ASCII) is caught by the context-overflow
// failover (armContextUpgrade) rather than paid for with a 4× over-count on all
// ordinary English. The design accepts that residual for the ASCII common case
// while keeping the non-ASCII path — where the real byte-fallback blow-ups
// happen — provably bounded.
//
// The weights are anchored to how BPE tokenizers actually behave in the worst
// case — BYTE FALLBACK. When a run of characters is outside the tokenizer's
// vocabulary it is emitted one token per UTF-8 byte, so a tokenizer can never
// charge MORE than one token per byte. The average-case [EstimateTokens] misses
// this by counting per character: a real auto-routed prompt of 61607 Chinese +
// symbol characters (237619 UTF-8 bytes) was charged 216543 tokens upstream —
// ~3.5 tokens per CHARACTER — and rejected against a 131072-context model,
// while 0.25/char had sized it at ~15k. Only a byte-anchored bound survives
// that content:
//
//   - Non-ASCII rune: weighted by its UTF-8 byte length (2–4). This is the
//     byte-fallback ceiling — CJK, emoji, and rare symbols that a model
//     tokenizes byte-by-byte can approach it, and no tokenizer exceeds it.
//     Common in-vocabulary CJK costs far less, so this over-counts it, which
//     only ever selects a larger model.
//   - ASCII rune: 0.5 tokens (≈2 chars/token). ASCII stays in vocabulary and
//     rarely byte-falls-back; 0.5 is double the "4 chars ≈ 1 token" prose
//     average and covers dense code / minified JSON / base64 (~0.3–0.4/char)
//     with headroom, without the 4× inflation a full byte count would impose
//     on ordinary English.
//
// Cost estimation and quota pre-charge do NOT use this — they want an accurate
// or already-conservative estimate for their own reasons (see
// estimator.pickTokenizer's family divisors and proxy.estimateTokens' bytes/3).
// This function is exclusively for the "will it fit?" question.
func EstimateTokensConservative(text string) int {
	if text == "" {
		return 0
	}
	var score float64
	for _, r := range text {
		score += runeTokenScoreConservative(r)
	}
	n := int(score)
	if score > float64(n) {
		n++
	}
	return n
}

// runeTokenScoreConservative is the upper-bound counterpart of runeTokenScore,
// anchored to the BPE byte-fallback ceiling (≤ 1 token per UTF-8 byte). See
// EstimateTokensConservative for why non-ASCII is weighted by byte length.
func runeTokenScoreConservative(r rune) float64 {
	if r < 128 {
		return 0.5
	}
	n := utf8.RuneLen(r)
	if n < 1 {
		// Invalid/unencodable rune (e.g. RuneError from malformed UTF-8):
		// charge the maximum UTF-8 width so we never under-count.
		n = 4
	}
	return float64(n)
}

// TruncateToTokens returns the longest TRAILING suffix of text whose estimated
// token count stays within maxTokens, applying a safety margin. It is the
// in-message hard cut behind Plan's default budget enforcement, and the
// last-resort cut for ReportOnly callers (L2 embedding input) that join
// Plan output into a single string: the strategies drop whole messages but
// never cut WITHIN one, so a single oversized message would otherwise be
// sent to the model over-limit and 400.
//
// It keeps the TAIL, not the head: the newest content (the latest user turn)
// sits at the end of the joined input and is what the embedding / classifier
// must reflect — dropping recent content to preserve an old system preamble
// would defeat the purpose. Oldest content (head) is discarded first.
//
// The cut is a FIT decision, so it counts with the CONSERVATIVE (upper-bound)
// per-rune weights, not the average-case ones: exceeding the real limit hard-
// 400s the provider (observed for L2 embedding input: dense content the
// average 0.25/char + 85% margin still let overflow an 8192-token cap). On top
// of the conservative count it keeps the ~85% target as a second margin, so
// the provider's real tokenizer is very unlikely to exceed the true limit. The
// "already fits" fast-path likewise uses the conservative estimate so it never
// skips a cut that the real tokenizer would need. Returns text unchanged when
// it already fits, when maxTokens <= 0, or when text is empty. The returned
// suffix always lands on a UTF-8 rune boundary.
func TruncateToTokens(text string, maxTokens int) string {
	if maxTokens <= 0 || text == "" {
		return text
	}
	if EstimateTokensConservative(text) <= maxTokens {
		return text
	}
	target := float64(maxTokens) * 0.85
	var score float64
	offset := len(text) // start byte of the kept suffix; len means "nothing yet"
	for offset > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:offset])
		if score+runeTokenScoreConservative(r) > target {
			break // including this older rune would exceed the budget — stop
		}
		score += runeTokenScoreConservative(r)
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
