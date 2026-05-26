package specutil

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestExtractCacheReadTokens_AllAliases asserts every known OpenAI-compat
// alias resolves to the same canonical Usage.CacheReadTokens. New aliases
// added to cachedTokenAliases must have a case here so drift surfaces.
func TestExtractCacheReadTokens_AllAliases(t *testing.T) {
	cases := []struct {
		name     string
		usage    string
		wantPtr  bool
		wantVal  int
		upstream string // documentation only
	}{
		{"OpenAI canonical", `{"prompt_tokens_details":{"cached_tokens":123}}`, true, 123, "OpenAI 2024-09+"},
		{"OpenAI Responses API", `{"input_tokens_details":{"cached_tokens":234}}`, true, 234, "OpenAI /v1/responses"},
		{"DeepSeek flat", `{"prompt_cache_hit_tokens":45}`, true, 45, "DeepSeek"},
		{"Moonshot explicit-cache", `{"prompt_cache_tokens":67}`, true, 67, "Moonshot v1 explicit-cache"},
		{"Kimi auto-prefix", `{"cached_tokens":89}`, true, 89, "Kimi K2/K2.5/K2.6"},
		{"none", `{"prompt_tokens":10}`, false, 0, "no cache info"},
		{"empty object", `{}`, false, 0, "absent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractCacheReadTokens(gjson.Parse(tc.usage))
			if tc.wantPtr {
				if got == nil {
					t.Fatalf("got nil, want pointer to %d (%s)", tc.wantVal, tc.upstream)
				}
				if *got != tc.wantVal {
					t.Errorf("got %d, want %d (%s)", *got, tc.wantVal, tc.upstream)
				}
			} else if got != nil {
				t.Errorf("got pointer to %d, want nil", *got)
			}
		})
	}
}

// TestExtractCacheReadTokens_PrecedenceOrder asserts the first-match-wins
// order in cachedTokenAliases. When two aliases coexist, the canonical
// OpenAI path wins so we never double-count or take a stale alias from
// a transitional provider response.
func TestExtractCacheReadTokens_PrecedenceOrder(t *testing.T) {
	// All four aliases populated; OpenAI canonical (first in the list)
	// must win.
	usage := `{
		"prompt_tokens_details":{"cached_tokens":1},
		"prompt_cache_hit_tokens":2,
		"prompt_cache_tokens":3,
		"cached_tokens":4
	}`
	got := ExtractCacheReadTokens(gjson.Parse(usage))
	if got == nil || *got != 1 {
		t.Errorf("precedence: got %v want pointer to 1", got)
	}
}

// TestExtractReasoningTokens pins the reasoning path. Single alias
// today; this test will need a new case row when a new family
// diverges from the OpenAI path.
func TestExtractReasoningTokens(t *testing.T) {
	cases := []struct {
		name    string
		usage   string
		wantPtr bool
		wantVal int
	}{
		{"OpenAI/DeepSeek/Moonshot", `{"completion_tokens_details":{"reasoning_tokens":42}}`, true, 42},
		{"OpenAI Responses API", `{"output_tokens_details":{"reasoning_tokens":77}}`, true, 77},
		{"absent", `{"completion_tokens":5}`, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractReasoningTokens(gjson.Parse(tc.usage))
			if tc.wantPtr {
				if got == nil || *got != tc.wantVal {
					t.Errorf("got %v want pointer to %d", got, tc.wantVal)
				}
			} else if got != nil {
				t.Errorf("got pointer to %d, want nil", *got)
			}
		})
	}
}

// TestExtractOpenAIUsage_Full asserts the combined extraction returns
// the full canonical Usage envelope for a representative OpenAI-style
// usage object, including the optional cache + reasoning fields.
func TestExtractOpenAIUsage_Full(t *testing.T) {
	usage := `{
		"prompt_tokens":100,
		"completion_tokens":50,
		"total_tokens":150,
		"prompt_tokens_details":{"cached_tokens":40},
		"completion_tokens_details":{"reasoning_tokens":20}
	}`
	u := ExtractOpenAIUsage(gjson.Parse(usage))
	if u.PromptTokens == nil || *u.PromptTokens != 100 {
		t.Errorf("PromptTokens: %v", u.PromptTokens)
	}
	if u.CompletionTokens == nil || *u.CompletionTokens != 50 {
		t.Errorf("CompletionTokens: %v", u.CompletionTokens)
	}
	if u.TotalTokens == nil || *u.TotalTokens != 150 {
		t.Errorf("TotalTokens: %v", u.TotalTokens)
	}
	if u.CacheReadTokens == nil || *u.CacheReadTokens != 40 {
		t.Errorf("CacheReadTokens: %v", u.CacheReadTokens)
	}
	if u.ReasoningTokens == nil || *u.ReasoningTokens != 20 {
		t.Errorf("ReasoningTokens: %v", u.ReasoningTokens)
	}
}

// TestExtractOpenAIUsage_ResponsesAPI pins that the OpenAI /v1/responses
// usage envelope — which renames prompt_tokens → input_tokens,
// completion_tokens → output_tokens, prompt_tokens_details →
// input_tokens_details, completion_tokens_details → output_tokens_details
// — extracts identically to the chat-completions shape. Drift here
// caused all prod Responses traffic to write NULL for cached/reasoning
// columns; the alias additions in this commit are guarded by this
// regression test.
func TestExtractOpenAIUsage_ResponsesAPI(t *testing.T) {
	usage := `{
		"input_tokens":100,
		"output_tokens":50,
		"total_tokens":150,
		"input_tokens_details":{"cached_tokens":40},
		"output_tokens_details":{"reasoning_tokens":20}
	}`
	u := ExtractOpenAIUsage(gjson.Parse(usage))
	if u.PromptTokens == nil || *u.PromptTokens != 100 {
		t.Errorf("PromptTokens: %v (want 100, from input_tokens alias)", u.PromptTokens)
	}
	if u.CompletionTokens == nil || *u.CompletionTokens != 50 {
		t.Errorf("CompletionTokens: %v (want 50, from output_tokens alias)", u.CompletionTokens)
	}
	if u.TotalTokens == nil || *u.TotalTokens != 150 {
		t.Errorf("TotalTokens: %v", u.TotalTokens)
	}
	if u.CacheReadTokens == nil || *u.CacheReadTokens != 40 {
		t.Errorf("CacheReadTokens: %v (want 40, from input_tokens_details.cached_tokens)", u.CacheReadTokens)
	}
	if u.ReasoningTokens == nil || *u.ReasoningTokens != 20 {
		t.Errorf("ReasoningTokens: %v (want 20, from output_tokens_details.reasoning_tokens)", u.ReasoningTokens)
	}
}

// TestExtractOpenAIUsage_AbsentObject asserts an absent usage object
// returns a zero-valued Usage (all nil pointers), not a zero-value
// envelope masquerading as "reported zeros".
func TestExtractOpenAIUsage_AbsentObject(t *testing.T) {
	u := ExtractOpenAIUsage(gjson.Result{})
	if u.PromptTokens != nil || u.CompletionTokens != nil || u.TotalTokens != nil ||
		u.CacheReadTokens != nil || u.ReasoningTokens != nil {
		t.Errorf("absent usage must yield zero-pointer Usage; got %+v", u)
	}
}

// TestPtrInt asserts PtrInt returns a non-nil pointer dereferencing to
// the input value. The helper is trivial but load-bearing: every
// extract-* function above relies on it producing a fresh address (not
// a stale shared pointer) so multiple Usage envelopes built in the
// same call don't alias each other.
func TestPtrInt(t *testing.T) {
	p1 := PtrInt(7)
	p2 := PtrInt(9)
	if p1 == nil || p2 == nil {
		t.Fatalf("PtrInt returned nil")
	}
	if *p1 != 7 || *p2 != 9 {
		t.Errorf("got *p1=%d *p2=%d, want 7 and 9", *p1, *p2)
	}
	if p1 == p2 {
		t.Errorf("PtrInt must return fresh pointers per call to avoid aliasing across Usage fields")
	}
}

// TestUsageFromOpenAI_AllPresent asserts the constructor populates all
// three pointer fields when every has-flag is true. This is the path
// every adapter takes on a fully-reported usage envelope.
func TestUsageFromOpenAI_AllPresent(t *testing.T) {
	u := UsageFromOpenAI(11, 22, 33, true, true, true)
	if u.PromptTokens == nil || *u.PromptTokens != 11 {
		t.Errorf("PromptTokens: %v", u.PromptTokens)
	}
	if u.CompletionTokens == nil || *u.CompletionTokens != 22 {
		t.Errorf("CompletionTokens: %v", u.CompletionTokens)
	}
	if u.TotalTokens == nil || *u.TotalTokens != 33 {
		t.Errorf("TotalTokens: %v", u.TotalTokens)
	}
}

// TestUsageFromOpenAI_PartialFlags asserts that a false has-flag leaves
// the corresponding field nil — NOT a pointer to zero. This distinction
// matters for traffic_event: a NULL column ("provider didn't report")
// is semantically different from 0 ("provider reported zero tokens"),
// and the AI Gateway cost pipeline branches on the pointer being nil.
func TestUsageFromOpenAI_PartialFlags(t *testing.T) {
	// Only prompt reported.
	u := UsageFromOpenAI(10, 0, 0, true, false, false)
	if u.PromptTokens == nil || *u.PromptTokens != 10 {
		t.Errorf("PromptTokens: %v", u.PromptTokens)
	}
	if u.CompletionTokens != nil {
		t.Errorf("CompletionTokens: want nil (not reported), got pointer to %d", *u.CompletionTokens)
	}
	if u.TotalTokens != nil {
		t.Errorf("TotalTokens: want nil (not reported), got pointer to %d", *u.TotalTokens)
	}

	// Only completion + total.
	u = UsageFromOpenAI(0, 5, 5, false, true, true)
	if u.PromptTokens != nil {
		t.Errorf("PromptTokens: want nil, got pointer to %d", *u.PromptTokens)
	}
	if u.CompletionTokens == nil || *u.CompletionTokens != 5 {
		t.Errorf("CompletionTokens: %v", u.CompletionTokens)
	}
	if u.TotalTokens == nil || *u.TotalTokens != 5 {
		t.Errorf("TotalTokens: %v", u.TotalTokens)
	}
}

// TestUsageFromOpenAI_AllAbsent asserts the all-false case yields a
// fully-zero envelope (no spurious pointers). This is what gets written
// when an adapter sees a response without a usage object at all.
func TestUsageFromOpenAI_AllAbsent(t *testing.T) {
	u := UsageFromOpenAI(99, 99, 99, false, false, false)
	if u.PromptTokens != nil || u.CompletionTokens != nil || u.TotalTokens != nil {
		t.Errorf("all-absent: want zero-pointer Usage, got %+v", u)
	}
}
