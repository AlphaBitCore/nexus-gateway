package proxy

import "testing"

// TestPreStampEmbeddingRequestMeta_StampsEstimatedPromptTokens verifies the
// request-side stamp records a positive local token estimate for both
// single-string and array inputs — the source for the no-usage cost fallback.
func TestPreStampEmbeddingRequestMeta_StampsEstimatedPromptTokens(t *testing.T) {
	single := preStampEmbeddingRequestMeta(nil, []byte(`{"model":"m","input":"hello world from the embedding test"}`), false)
	if got := embeddingEstimatedPromptTokens(single); got <= 0 {
		t.Errorf("single-input estimate = %d, want > 0", got)
	}

	batch := preStampEmbeddingRequestMeta(nil, []byte(`{"model":"m","input":["alpha beta gamma","delta epsilon zeta eta"]}`), false)
	if got := embeddingEstimatedPromptTokens(batch); got <= 0 {
		t.Errorf("array-input estimate = %d, want > 0", got)
	}
}

// TestEmbeddingTokenFallback locks the no-usage cost fallback the proxy applies
// on the non-stream embeddings upstream paths (live + broker-subscription):
// when the upstream reported no token usage (e.g. Gemini embedContent) the
// request-side estimate is substituted; otherwise the real count is preserved.
// Embeddings are never served from the response cache, so there is no
// cache-HIT fallback path for this endpoint.
func TestEmbeddingTokenFallback(t *testing.T) {
	meta := preStampEmbeddingRequestMeta(nil, []byte(`{"model":"m","input":"some words here to count"}`), false)
	est := int64(embeddingEstimatedPromptTokens(meta))
	if est <= 0 {
		t.Fatalf("precondition: estimate = %d, want > 0", est)
	}

	cases := []struct {
		name     string
		endpoint string
		current  int64
		metadata any
		want     int64
	}{
		{"embeddings_no_usage_uses_estimate", "embeddings", 0, meta, est},
		{"embeddings_real_usage_preserved", "embeddings", 40, meta, 40},
		{"chat_never_overridden", "chat", 0, meta, 0},
		{"embeddings_no_estimate_unchanged", "embeddings", 0, map[string]any{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := embeddingTokenFallback(c.endpoint, c.current, c.metadata); got != c.want {
				t.Errorf("embeddingTokenFallback(%q, %d, …) = %d, want %d", c.endpoint, c.current, got, c.want)
			}
		})
	}
}

// TestEmbeddingEstimatedPromptTokens_Types covers every numeric type the
// JSONB-decoded estimate can arrive as (int in-memory, int64/float64 after a
// JSON round-trip) plus the absent / wrong-shape paths.
func TestEmbeddingEstimatedPromptTokens_Types(t *testing.T) {
	mk := func(v any) any {
		return map[string]any{"embedding": map[string]any{"estimated_prompt_tokens": v}}
	}
	cases := []struct {
		name string
		meta any
		want int
	}{
		{"int", mk(7), 7},
		{"int64", mk(int64(9)), 9},
		{"float64", mk(float64(11)), 11},
		{"wrong-type", mk("nope"), 0},
		{"no-embedding-submap", map[string]any{"other": 1}, 0},
		{"not-a-map", "scalar", 0},
		{"nil", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := embeddingEstimatedPromptTokens(c.meta); got != c.want {
				t.Errorf("embeddingEstimatedPromptTokens(%v) = %d, want %d", c.meta, got, c.want)
			}
		})
	}

	// embeddingTokenFallback returns the estimate (as int64) for the
	// embeddings + zero-usage + int64-estimate combination.
	if got := embeddingTokenFallback("embeddings", 0, map[string]any{"embedding": map[string]any{"estimated_prompt_tokens": int64(13)}}); got != 13 {
		t.Errorf("embeddingTokenFallback int64 estimate = %d, want 13", got)
	}
}
