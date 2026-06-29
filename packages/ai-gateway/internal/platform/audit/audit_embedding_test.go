// Package audit — audit_embedding_test.go covers the authoritative coerce
// behaviour of coerceEmbeddingRow.
//
// coerceEmbeddingRow runs from Writer.Enqueue gated on EndpointType ==
// EndpointTypeEmbeddings. It is the single source of truth — every producer
// (proxy live response, proxy cache hits, ai-guard sink) reaches the audit
// boundary through Enqueue, so the coerce is uniform.
//
// Named failure modes:
//   - CompletionTokens warns + zeroed for embeddings endpoint
//   - CacheReadTokens warns + zeroed for embeddings endpoint
//   - CacheCreationTokens warns + zeroed for embeddings endpoint
//   - ReasoningTokens warns + zeroed for embeddings endpoint
//   - ReasoningCostUsd warns + zeroed for embeddings endpoint
//   - clean embedding row produces no warning + no mutation
//   - non-embedding endpoint never triggers the coerce
//   - multi-field violation emits one warning per field, all zeroed
package audit

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// warnLogger builds an slog.Logger backed by a text handler that writes to buf.
func warnLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestCoerceEmbeddingRow_completionTokensWarnsAndZeroes(t *testing.T) {
	w, rec, logBuf := setupCoerceTest()
	rec.EndpointType = "embeddings"
	rec.CompletionTokens = 42

	w.Enqueue(rec)

	if rec.CompletionTokens != 0 {
		t.Errorf("CompletionTokens = %d after coerce, want 0", rec.CompletionTokens)
	}
	if !strings.Contains(logBuf.String(), "completion_tokens") {
		t.Errorf("expected warning about completion_tokens, got: %s", logBuf.String())
	}
}

func TestCoerceEmbeddingRow_cacheReadTokensWarnsAndZeroes(t *testing.T) {
	w, rec, logBuf := setupCoerceTest()
	rec.EndpointType = "embeddings"
	rec.CacheReadTokens = 10

	w.Enqueue(rec)

	if rec.CacheReadTokens != 0 {
		t.Errorf("CacheReadTokens = %d after coerce, want 0", rec.CacheReadTokens)
	}
	if !strings.Contains(logBuf.String(), "cache_read_tokens") {
		t.Errorf("expected warning about cache_read_tokens, got: %s", logBuf.String())
	}
}

func TestCoerceEmbeddingRow_cacheCreationTokensWarnsAndZeroes(t *testing.T) {
	w, rec, logBuf := setupCoerceTest()
	rec.EndpointType = "embeddings"
	rec.CacheCreationTokens = 5

	w.Enqueue(rec)

	if rec.CacheCreationTokens != 0 {
		t.Errorf("CacheCreationTokens = %d after coerce, want 0", rec.CacheCreationTokens)
	}
	if !strings.Contains(logBuf.String(), "cache_creation_tokens") {
		t.Errorf("expected warning about cache_creation_tokens, got: %s", logBuf.String())
	}
}

func TestCoerceEmbeddingRow_reasoningTokensWarnsAndZeroes(t *testing.T) {
	w, rec, logBuf := setupCoerceTest()
	rec.EndpointType = "embeddings"
	rec.ReasoningTokens = 100

	w.Enqueue(rec)

	if rec.ReasoningTokens != 0 {
		t.Errorf("ReasoningTokens = %d after coerce, want 0", rec.ReasoningTokens)
	}
	if !strings.Contains(logBuf.String(), "reasoning_tokens") {
		t.Errorf("expected warning about reasoning_tokens, got: %s", logBuf.String())
	}
}

func TestCoerceEmbeddingRow_reasoningCostUsdWarnsAndZeroes(t *testing.T) {
	w, rec, logBuf := setupCoerceTest()
	rec.EndpointType = "embeddings"
	rec.ReasoningCostUsd = 0.001

	w.Enqueue(rec)

	if rec.ReasoningCostUsd != 0 {
		t.Errorf("ReasoningCostUsd = %f after coerce, want 0", rec.ReasoningCostUsd)
	}
	if !strings.Contains(logBuf.String(), "reasoning_cost_usd") {
		t.Errorf("expected warning about reasoning_cost_usd, got: %s", logBuf.String())
	}
}

func TestCoerceEmbeddingRow_allZeroNoWarning(t *testing.T) {
	w, rec, logBuf := setupCoerceTest()
	rec.EndpointType = "embeddings"
	rec.PromptTokens = 500
	rec.TotalTokens = 500

	w.Enqueue(rec)

	if logBuf.Len() > 0 {
		t.Errorf("expected no log output for clean embedding row, got: %s", logBuf.String())
	}
}

func TestCoerceEmbeddingRow_nonEmbeddingEndpointSkipsCoerce(t *testing.T) {
	w, rec, logBuf := setupCoerceTest()
	rec.EndpointType = "chat"
	rec.CompletionTokens = 200 // valid for chat endpoint

	w.Enqueue(rec)

	if rec.CompletionTokens != 200 {
		t.Errorf("CompletionTokens should be unchanged for chat endpoint, got %d", rec.CompletionTokens)
	}
	if logBuf.Len() > 0 {
		t.Errorf("no warning expected for chat endpoint, got: %s", logBuf.String())
	}
}

func TestCoerceEmbeddingRow_multipleFieldsAllWarnedAllZeroed(t *testing.T) {
	w, rec, logBuf := setupCoerceTest()
	rec.EndpointType = "embeddings"
	rec.CompletionTokens = 5
	rec.ReasoningTokens = 3
	rec.CacheReadTokens = 2

	w.Enqueue(rec)

	if rec.CompletionTokens != 0 {
		t.Error("CompletionTokens not zeroed")
	}
	if rec.ReasoningTokens != 0 {
		t.Error("ReasoningTokens not zeroed")
	}
	if rec.CacheReadTokens != 0 {
		t.Error("CacheReadTokens not zeroed")
	}
	logs := logBuf.String()
	for _, field := range []string{"completion_tokens", "reasoning_tokens", "cache_read_tokens"} {
		if !strings.Contains(logs, field) {
			t.Errorf("expected warning for %s in: %s", field, logs)
		}
	}
}

// setupCoerceTest creates a no-op Writer (nil producer) with a log buffer
// wired, plus a minimal Record. Returns the writer, record, and log buffer
// so callers can assert on log output.
func setupCoerceTest() (*Writer, *Record, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	logger := warnLogger(buf)
	w := NewWriter(nil, "test.queue", nil, logger)
	// The coerce runs synchronously in Enqueue BEFORE ensureStarted; claim startOnce
	// so Enqueue's lazy ensureStarted() does not spin up a consumer worker that would
	// log to buf from a background goroutine, racing the test's logBuf.String() read.
	w.startOnce.Do(func() {})
	w.recCh = make(chan *Record, 1)
	rec := &Record{RequestID: "req-embed-test"}
	return w, rec, buf
}
