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
// boundary for multibyte (CJK) input.
func TestTruncateToTokens_BudgetAndRuneBoundary(t *testing.T) {
	in := strings.Repeat("你好", 500) // 1000 CJK runes ≈ 500 tokens
	const maxTokens = 50
	out := TruncateToTokens(in, maxTokens)

	if EstimateTokens(out) > maxTokens {
		t.Fatalf("estimated tokens %d exceed budget %d", EstimateTokens(out), maxTokens)
	}
	if EstimateTokens(out) > maxTokens*85/100+1 {
		t.Errorf("expected ~85%% margin, got %d tokens for budget %d", EstimateTokens(out), maxTokens)
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
