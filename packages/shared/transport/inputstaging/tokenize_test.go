package inputstaging

import (
	"strings"
	"testing"
)

func TestEstimateTokens_Empty(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("EstimateTokens(%q) = %d, want 0", "", got)
	}
}

func TestEstimateTokens_ASCIIApproximation(t *testing.T) {
	// 4 ASCII characters should yield approximately 1 token.
	// "test" is exactly 4 chars; heuristic gives 4 * 0.25 = 1.0 → 1.
	got := EstimateTokens("test")
	if got != 1 {
		t.Errorf("EstimateTokens(%q) = %d, want 1", "test", got)
	}

	// 8 ASCII chars → 2 tokens.
	got = EstimateTokens("testtest")
	if got != 2 {
		t.Errorf("EstimateTokens(%q) = %d, want 2", "testtest", got)
	}
}

func TestEstimateTokens_RoundUp(t *testing.T) {
	// 1 ASCII character → 0.25 score → rounds up to 1.
	got := EstimateTokens("a")
	if got < 1 {
		t.Errorf("EstimateTokens(%q) = %d, want >= 1 (non-empty text never returns 0)", "a", got)
	}
}

func TestEstimateTokens_EnglishParagraph(t *testing.T) {
	// A typical English sentence of ~40 words / ~200 chars → ~50 tokens.
	text := strings.Repeat("Hello world, this is a test sentence. ", 5)
	got := EstimateTokens(text)
	// Rough expectation: len(text)/4 ± margin.
	lo, hi := len(text)/5, len(text)/3
	if got < lo || got > hi {
		t.Errorf("EstimateTokens(paragraph) = %d, want in [%d, %d]", got, lo, hi)
	}
}

func TestEstimateTokens_CJK(t *testing.T) {
	// CJK characters score at 0.5 tokens each.
	// "你好" = 2 CJK chars → 2 * 0.5 = 1.0 → 1 token.
	got := EstimateTokens("你好")
	if got != 1 {
		t.Errorf("EstimateTokens(%q) = %d, want 1", "你好", got)
	}

	// 4 CJK chars → 4 * 0.5 = 2.0 → 2 tokens.
	got = EstimateTokens("你好世界")
	if got != 2 {
		t.Errorf("EstimateTokens(%q) = %d, want 2", "你好世界", got)
	}
}

func TestEstimateTokens_Hiragana(t *testing.T) {
	// Hiragana is also in the CJK-like set.
	// "あいう" = 3 hiragana → 3 * 0.5 = 1.5 → rounds up to 2.
	got := EstimateTokens("あいう")
	if got != 2 {
		t.Errorf("EstimateTokens(%q) = %d, want 2", "あいう", got)
	}
}

func TestEstimateTokens_Katakana(t *testing.T) {
	// Katakana: "アイウ" = 3 chars → 1.5 → 2.
	got := EstimateTokens("アイウ")
	if got != 2 {
		t.Errorf("EstimateTokens(%q) = %d, want 2", "アイウ", got)
	}
}

func TestEstimateTokens_Hangul(t *testing.T) {
	// Hangul: "안녕하세요" = 5 chars → 5 * 0.5 = 2.5 → 3.
	got := EstimateTokens("안녕하세요")
	if got != 3 {
		t.Errorf("EstimateTokens(%q) = %d, want 3", "안녕하세요", got)
	}
}

func TestEstimateTokens_Mixed(t *testing.T) {
	// Mixed ASCII + CJK.
	// "Hi你好" = 2 ASCII (0.25 each) + 2 CJK (0.5 each) = 0.5 + 1.0 = 1.5 → 2.
	got := EstimateTokens("Hi你好")
	if got != 2 {
		t.Errorf("EstimateTokens(%q) = %d, want 2", "Hi你好", got)
	}
}

func TestEstimateTokens_LargeInput(t *testing.T) {
	// 4000 ASCII chars → 1000 tokens.
	text := strings.Repeat("a", 4000)
	got := EstimateTokens(text)
	if got != 1000 {
		t.Errorf("EstimateTokens(4000 ASCII chars) = %d, want 1000", got)
	}
}

func TestEstimateTokens_Whitespace(t *testing.T) {
	// Spaces are ASCII (< 128), so 4 spaces → 0.25 * 4 = 1.0 → 1 token.
	got := EstimateTokens("    ")
	if got != 1 {
		t.Errorf("EstimateTokens(%q) = %d, want 1", "    ", got)
	}
}

func TestIsCJKLike_Han(t *testing.T) {
	for _, r := range "你好世界" {
		if !isCJKLike(r) {
			t.Errorf("isCJKLike(%q) = false, want true", r)
		}
	}
}

func TestIsCJKLike_ASCII(t *testing.T) {
	for _, r := range "Hello" {
		if isCJKLike(r) {
			t.Errorf("isCJKLike(%q) = true, want false", r)
		}
	}
}

func TestEstimateTokens_OtherUnicode(t *testing.T) {
	// Latin Extended characters (e.g. "é", "ñ") are non-ASCII, non-CJK.
	// They score at 0.4 tokens each.
	// "café" = 'c'(0.25) + 'a'(0.25) + 'f'(0.25) + 'é'(0.4) = 1.15 → 2.
	got := EstimateTokens("café")
	if got < 1 {
		t.Errorf("EstimateTokens(%q) = %d, want >= 1", "café", got)
	}
	// "résumé" = 'r','é','s','u','m','é' = 4 ASCII + 2 other = 4*0.25 + 2*0.4 = 1.8 → 2.
	got = EstimateTokens("résumé")
	if got < 1 {
		t.Errorf("EstimateTokens(%q) = %d, want >= 1", "résumé", got)
	}
}

func TestEstimateTokensConservative_Empty(t *testing.T) {
	if got := EstimateTokensConservative(""); got != 0 {
		t.Errorf("EstimateTokensConservative(%q) = %d, want 0", "", got)
	}
}

// The whole point of the conservative estimate is that it never lands BELOW
// the average-case estimate — for every script, for every input.
func TestEstimateTokensConservative_NeverBelowAverage(t *testing.T) {
	samples := []string{
		"a",
		"test",
		"Hello world, this is a test sentence.",
		strings.Repeat("Hello world ", 100),
		"你好世界",
		strings.Repeat("你好", 200),
		"café résumé naïve",
		`{"name":"x","values":[1,2,3],"nested":{"deep":true}}`,
		"Hi你好café",
	}
	for _, s := range samples {
		cons, avg := EstimateTokensConservative(s), EstimateTokens(s)
		if cons < avg {
			t.Errorf("EstimateTokensConservative(%q) = %d < EstimateTokens = %d; conservative must never under-count", s, cons, avg)
		}
	}
}

// The bug this exists to end: dense content (code/JSON/tool schemas) tokenizes
// at ~0.32 tokens/char, well above the average-case 0.25. A real auto-routed
// prompt of 670k ASCII chars counted 216543 tokens upstream (≈0.323/char) while
// the 0.25 estimate said ~167k, routing it to a too-small model. The
// conservative estimate must cover that observed density.
func TestEstimateTokensConservative_CoversObservedDenseAscii(t *testing.T) {
	const observedDenseTokensPerChar = 0.323 // real upstream count / char count
	body := strings.Repeat("a", 670000)
	got := EstimateTokensConservative(body)
	floor := int(float64(len(body)) * observedDenseTokensPerChar)
	if got < floor {
		t.Errorf("EstimateTokensConservative(670k ASCII) = %d, want >= %d (observed dense density) — would still under-count and mis-route", got, floor)
	}
	// The average-case estimate demonstrably would NOT cover it — this is the regression guard.
	if EstimateTokens(body) >= floor {
		t.Fatalf("test premise broken: average EstimateTokens(%d) already covers dense floor %d", EstimateTokens(body), floor)
	}
}

// The incident that reset this whole estimate: a BPE tokenizer that lacks a
// character in its vocabulary emits one token per UTF-8 BYTE (byte fallback),
// so 3-byte CJK / 4-byte symbols can cost up to 3–4 tokens PER CHARACTER, not
// the ~0.5–1 a per-character heuristic assumes. A real 61607-char Chinese +
// alchemical-symbol prompt (237619 UTF-8 bytes) was charged 216543 tokens
// upstream — ~3.5 tokens/char — and the per-character estimate sized it at
// ~15k, mis-routing it to a 131072-context model. The conservative estimate
// must weight non-ASCII by BYTE length so it never lands under that.
func TestEstimateTokensConservative_CoversByteFallback(t *testing.T) {
	const runes = 1000
	body := strings.Repeat("参", runes) // U+53C2, 3 UTF-8 bytes
	byteLen := len([]byte(body))       // 3000
	got := EstimateTokensConservative(body)
	if got < byteLen {
		t.Errorf("EstimateTokensConservative(%d CJK runes) = %d, want >= %d (UTF-8 byte count = byte-fallback ceiling)", runes, got, byteLen)
	}
	// The per-character average would sit far below the byte-fallback token
	// count (0.5/char = 500 for 1000 runes) — the regression this guards.
	if EstimateTokens(body) >= byteLen {
		t.Fatalf("test premise broken: average EstimateTokens(%d) already covers byte ceiling %d", EstimateTokens(body), byteLen)
	}
	// 4-byte symbols (the alchemical glyphs in the real prompt) must weight 4.
	emoji := "\U0001F701" // 4 UTF-8 bytes
	if got := EstimateTokensConservative(emoji); got < 4 {
		t.Errorf("EstimateTokensConservative(4-byte rune) = %d, want >= 4", got)
	}
}

func TestEstimateTokensConservative_RoundUp(t *testing.T) {
	// 4 ASCII chars → 4 * 0.5 = 2.0 → 2 tokens.
	if got := EstimateTokensConservative("test"); got != 2 {
		t.Errorf("EstimateTokensConservative(%q) = %d, want 2", "test", got)
	}
	// 2 CJK chars @ 3 bytes each → 2 * 3 = 6 tokens.
	if got := EstimateTokensConservative("你好"); got != 6 {
		t.Errorf("EstimateTokensConservative(%q) = %d, want 6", "你好", got)
	}
}

func TestTruncateToTokens_FitsUnchanged(t *testing.T) {
	in := "hello world"
	if got := TruncateToTokens(in, 1000); got != in {
		t.Errorf("fits-within-budget should be unchanged: got %q", got)
	}
}

func TestTruncateToTokens_EdgeCases(t *testing.T) {
	if got := TruncateToTokens("", 10); got != "" {
		t.Errorf("empty: got %q", got)
	}
	if got := TruncateToTokens("anything", 0); got != "anything" {
		t.Errorf("maxTokens=0 → unchanged: got %q", got)
	}
	if got := TruncateToTokens("anything", -5); got != "anything" {
		t.Errorf("maxTokens<0 → unchanged: got %q", got)
	}
}

// The cut must KEEP THE TAIL (newest content) and discard the old head.
func TestTruncateToTokens_KeepsNewestTail(t *testing.T) {
	old := strings.Repeat("O", 400)
	newest := "THE-NEWEST-QUESTION"
	in := old + newest
	out := TruncateToTokens(in, 20)

	if out == in {
		t.Fatal("expected truncation, got the full string")
	}
	if !strings.HasSuffix(in, out) {
		t.Fatalf("result must be a trailing SUFFIX of the input; got %q", out)
	}
	if !strings.HasSuffix(out, newest) {
		t.Fatalf("must retain the newest tail %q; got %q", newest, out)
	}
	if strings.Contains(out, strings.Repeat("O", 200)) {
		t.Fatalf("should have dropped the old head, but a large old run survived: %q", out)
	}
}

// Result must stay within budget (with the safety margin) and on a rune
// boundary for multibyte (CJK) input. TruncateToTokens is a fit cut, so the
// budget is judged with the CONSERVATIVE estimate it uses internally — the
// average estimate would let the real tokenizer overflow.
func TestTruncateToTokens_BudgetAndRuneBoundary(t *testing.T) {
	in := strings.Repeat("你好", 500) // 1000 CJK runes @ 3 bytes ≈ 3000 conservative tokens
	const maxTokens = 50
	out := TruncateToTokens(in, maxTokens)

	if EstimateTokensConservative(out) > maxTokens {
		t.Fatalf("conservative tokens %d exceed budget %d", EstimateTokensConservative(out), maxTokens)
	}
	if EstimateTokensConservative(out) > maxTokens*85/100+1 {
		t.Errorf("expected ~85%% margin, got %d conservative tokens for budget %d", EstimateTokensConservative(out), maxTokens)
	}
	for _, r := range out {
		if r == '�' {
			t.Fatal("result contains a broken UTF-8 rune (not on a boundary)")
		}
	}
	if !strings.HasSuffix(in, out) {
		t.Fatal("CJK result must also be a trailing suffix")
	}
}
