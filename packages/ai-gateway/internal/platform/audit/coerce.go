package audit

import "log/slog"

// coerceEmbeddingRow is the single authoritative coerce point for chat-only
// fields on embedding audit records. Called from Writer.Enqueue gated on
// EndpointType == EndpointTypeEmbeddings, so it runs uniformly for every
// producer that emits through the audit Writer (proxy live response, proxy
// cache hits, ai-guard classify rows that ever target an embedding model).
//
// For each forbidden field that's non-zero, logs a warning naming the field
// + value + requestId so the codec regression is visible to operators
// triaging the alert, then zeroes the field so the wire record matches the
// schema contract for embedding rows (no completion / cache / reasoning
// tokens).
func coerceEmbeddingRow(rec *Record, logger *slog.Logger) {
	if rec.CompletionTokens != 0 {
		logger.Warn("embedding row carried chat-only field; zeroing at audit boundary",
			"field", "completion_tokens", "value", rec.CompletionTokens, "requestId", rec.RequestID)
		rec.CompletionTokens = 0
	}
	if rec.CacheReadTokens != 0 {
		logger.Warn("embedding row carried chat-only field; zeroing at audit boundary",
			"field", "cache_read_tokens", "value", rec.CacheReadTokens, "requestId", rec.RequestID)
		rec.CacheReadTokens = 0
	}
	if rec.CacheCreationTokens != 0 {
		logger.Warn("embedding row carried chat-only field; zeroing at audit boundary",
			"field", "cache_creation_tokens", "value", rec.CacheCreationTokens, "requestId", rec.RequestID)
		rec.CacheCreationTokens = 0
	}
	if rec.ReasoningTokens != 0 {
		logger.Warn("embedding row carried chat-only field; zeroing at audit boundary",
			"field", "reasoning_tokens", "value", rec.ReasoningTokens, "requestId", rec.RequestID)
		rec.ReasoningTokens = 0
	}
	if rec.ReasoningCostUsd != 0 {
		logger.Warn("embedding row carried chat-only field; zeroing at audit boundary",
			"field", "reasoning_cost_usd", "value", rec.ReasoningCostUsd, "requestId", rec.RequestID)
		rec.ReasoningCostUsd = 0
	}
}
