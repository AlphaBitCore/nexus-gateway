package ingress

import (
	"context"
	"log/slog"
	"sync"
)

// Counters is the Anthropic Messages wire's usage triple.
type Counters struct {
	InputTokens         int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	// Set when the upstream's cache counters sum past the total it also
	// reported, which no additive triple can express.
	Contradictory bool
}

// Canonical follows OpenAI: prompt_tokens is the TOTAL, cached_tokens a subset
// inside it. Anthropic's wire is ADDITIVE — input_tokens counts only tokens
// neither read from nor written to the cache, and a client sums the three.
//
// Measured in production before this was fixed: a request whose real uncached
// input was 12 tokens came back as input_tokens=12936 beside
// cache_creation_input_tokens=12924, which an SDK adds up to 25860.
//
// Both the streaming and non-streaming arms call this. Gemini is deliberately
// NOT converted — its promptTokenCount is already a total with
// cachedContentTokenCount as a subset.
func ToAnthropicCounters(promptTokens, cacheRead, cacheCreation int64) Counters {
	// Subtracting a negative ADDS: prompt_tokens 10 with cached_tokens -5
	// produced input_tokens 15, above the total the same response reported.
	promptTokens = atLeastZero(promptTokens)
	cacheRead = atLeastZero(cacheRead)
	cacheCreation = atLeastZero(cacheCreation)

	if cacheRead+cacheCreation > promptTokens {
		// Clamping the uncached side to zero and emitting the counters anyway
		// would sum to MORE than the provider's own total. Instead input_tokens
		// carries that total and the cache counters are omitted — their absence
		// is the signal that the breakdown could not be reconciled.
		return Counters{InputTokens: promptTokens, Contradictory: true}
	}
	return Counters{
		InputTokens:         promptTokens - cacheRead - cacheCreation,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheCreation,
	}
}

func atLeastZero(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// Without this the row carries a plausible number and no sign that the gateway
// could not reconcile what it was told.
func WarnContradictoryUsage(model string, prompt, cacheRead, cacheCreation int64) {
	key := model
	if _, loaded := seenContradiction.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	slog.Default().LogAttrs(context.Background(), slog.LevelWarn,
		"nexus: upstream reported cache tokens exceeding its own prompt total",
		slog.String("event", "nexus_usage_contradiction"),
		slog.String("model", model),
		slog.Int64("prompt_tokens", prompt),
		slog.Int64("cache_read_tokens", cacheRead),
		slog.Int64("cache_creation_tokens", cacheCreation),
	)
}

// One entry per model: a misreporting upstream misreports every request.
var seenContradiction sync.Map
