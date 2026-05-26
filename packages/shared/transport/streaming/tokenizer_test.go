package streaming

import (
	"context"
	"testing"
)

func TestHeuristicGPTCount(t *testing.T) {
	cases := map[string]int{
		"":      0,
		"a":     1, // minimum 1 for non-empty
		"hello": 2, // 5/4 = 1.25 → ceil = 2
		"1234":  1, // exactly 4 chars → 1
		"12345": 2, // 5 chars → 2
		"你好":    1, // 2 runes → 1
	}
	tok := heuristicGPT{}
	for text, want := range cases {
		got, err := tok.Count(context.Background(), text)
		if err != nil {
			t.Errorf("Count(%q) err = %v", text, err)
		}
		if got != want {
			t.Errorf("Count(%q) = %d, want %d", text, got, want)
		}
	}
}

func TestHeuristicAnthropicDensity(t *testing.T) {
	// Claude is denser than GPT — same text should produce fewer tokens
	// than the GPT heuristic (~20% denser per Anthropic benchmarks).
	text := "The quick brown fox jumps over the lazy dog"
	gpt, _ := heuristicGPT{}.Count(context.Background(), text)
	ant, _ := heuristicAnthropic{}.Count(context.Background(), text)
	if ant >= gpt {
		t.Errorf("anthropic heuristic should be < gpt: ant=%d gpt=%d", ant, gpt)
	}
}

func TestTokenizerFor(t *testing.T) {
	for _, p := range []string{"openai", "azure", "deepseek", "glm", "minimax"} {
		if _, ok := tokenizerFor(p).(heuristicGPT); !ok {
			t.Errorf("tokenizerFor(%q) should be heuristicGPT", p)
		}
	}
	if _, ok := tokenizerFor("anthropic").(heuristicAnthropic); !ok {
		t.Errorf("tokenizerFor(anthropic) should be heuristicAnthropic")
	}
	if _, ok := tokenizerFor("gemini").(heuristicSentencePiece); !ok {
		t.Errorf("tokenizerFor(gemini) should be heuristicSentencePiece")
	}
	// Unknown provider → fallback to heuristicGPT (never nil).
	if tokenizerFor("unknown") == nil {
		t.Error("tokenizerFor(unknown) should not be nil")
	}
}

func TestCountWithDeadlineEmpty(t *testing.T) {
	n, err := countWithDeadline(context.Background(), heuristicGPT{}, "")
	if err != nil {
		t.Errorf("empty text err = %v", err)
	}
	if n != 0 {
		t.Errorf("empty text count = %d, want 0", n)
	}
}

func TestCountWithDeadlineNilTokenizer(t *testing.T) {
	if _, err := countWithDeadline(context.Background(), nil, "hello"); err == nil {
		t.Error("nil tokenizer should return error")
	}
}
