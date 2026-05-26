package metrics_test

import (
	"math"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

func iptr(v int) *int     { return &v }
func fptr(v float64) *float64 { return &v }

// TestCalculateCost_AnthropicCacheMix verifies the four-component
// cost calculation for the canonical Anthropic cache scenario: prompt
// includes uncached + cache_read + cache_creation, output is billed
// separately. PromptTokens carries the OpenAI-normalized total.
func TestCalculateCost_AnthropicCacheMix(t *testing.T) {
	// 50 uncached + 4000 cache_read + 1000 cache_write = 5050 total input.
	// 20 output tokens.
	// Prices: input=$3/M, output=$15/M, cacheRead=$0.30/M (0.1×), cacheWrite=$3.75/M (1.25×).
	u := provcore.Usage{
		PromptTokens:        iptr(5050),
		CompletionTokens:    iptr(20),
		TotalTokens:         iptr(5070),
		CacheReadTokens:     iptr(4000),
		CacheCreationTokens: iptr(1000),
	}
	p := metrics.ModelPrices{
		InputUsdPerM:            fptr(3.00),
		OutputUsdPerM:           fptr(15.00),
		CachedInputReadUsdPerM:  fptr(0.30),
		CachedInputWriteUsdPerM: fptr(3.75),
	}
	got := metrics.CalculateCost(u, p)

	want := metrics.Cost{
		UncachedInput: 50 * 3.00 / 1e6,           // 0.00015
		CacheRead:     4000 * 0.30 / 1e6,         // 0.0012
		CacheWrite:    1000 * 3.75 / 1e6,         // 0.00375
		Output:        20 * 15.00 / 1e6,          // 0.0003
	}
	want.Total = want.UncachedInput + want.CacheRead + want.CacheWrite + want.Output

	assertClose(t, "UncachedInput", got.UncachedInput, want.UncachedInput)
	assertClose(t, "CacheRead", got.CacheRead, want.CacheRead)
	assertClose(t, "CacheWrite", got.CacheWrite, want.CacheWrite)
	assertClose(t, "Output", got.Output, want.Output)
	assertClose(t, "Total", got.Total, want.Total)
}

// TestCalculateCost_NoCachePrices_FallbackToInputPrice verifies that when
// the cache prices are not configured (most non-Anthropic upstreams),
// cache_read and cache_write tokens are billed at the standard input
// rate — preserving the "no discount available" semantics.
func TestCalculateCost_NoCachePrices_FallbackToInputPrice(t *testing.T) {
	u := provcore.Usage{
		PromptTokens:    iptr(1000),
		CompletionTokens: iptr(100),
		CacheReadTokens: iptr(200),
	}
	p := metrics.ModelPrices{
		InputUsdPerM:  fptr(2.00),
		OutputUsdPerM: fptr(10.00),
		// CachedInputReadUsdPerM nil — fall back to InputUsdPerM.
	}
	got := metrics.CalculateCost(u, p)
	// UncachedInput = 1000 - 200 - 0 = 800; CacheRead is billed at 2.00 (fallback).
	assertClose(t, "UncachedInput", got.UncachedInput, 800*2.00/1e6)
	assertClose(t, "CacheRead", got.CacheRead, 200*2.00/1e6)
	assertClose(t, "Total", got.Total, 800*2.00/1e6 + 200*2.00/1e6 + 100*10.00/1e6)
}

// TestCalculateCost_ReasoningSplit verifies the reasoning subset of
// Output is surfaced as Cost.ReasoningSplit — already counted inside
// Output, exposed for analytics.
func TestCalculateCost_ReasoningSplit(t *testing.T) {
	u := provcore.Usage{
		PromptTokens:     iptr(100),
		CompletionTokens: iptr(500), // includes 300 reasoning tokens
		ReasoningTokens:  iptr(300),
	}
	p := metrics.ModelPrices{
		InputUsdPerM:  fptr(1.00),
		OutputUsdPerM: fptr(10.00),
	}
	got := metrics.CalculateCost(u, p)
	assertClose(t, "Output", got.Output, 500*10.00/1e6)
	assertClose(t, "ReasoningSplit", got.ReasoningSplit, 300*10.00/1e6)
	// ReasoningSplit is SUBSET of Output, NOT added on top.
	assertClose(t, "Total", got.Total, 100*1.00/1e6 + 500*10.00/1e6)
}

// TestCalculateCost_UncachedUnderflowDefense verifies the defensive
// clamp: if a provider misreports such that PromptTokens < CacheRead +
// CacheWrite (shouldn't happen but observed in malformed Anthropic
// pre-normalization paths), UncachedInput floors to 0 instead of going
// negative.
func TestCalculateCost_UncachedUnderflowDefense(t *testing.T) {
	u := provcore.Usage{
		PromptTokens:        iptr(100),
		CompletionTokens:    iptr(50),
		CacheReadTokens:     iptr(80),
		CacheCreationTokens: iptr(50), // 80 + 50 = 130 > 100, should clamp
	}
	p := metrics.ModelPrices{
		InputUsdPerM:  fptr(1.00),
		OutputUsdPerM: fptr(5.00),
	}
	got := metrics.CalculateCost(u, p)
	if got.UncachedInput != 0 {
		t.Errorf("UncachedInput should clamp to 0 on underflow, got %v", got.UncachedInput)
	}
}

// TestCalculateCost_NilUsage verifies graceful handling of fully-nil
// Usage (no upstream report) — returns zero Cost, never NaN.
func TestCalculateCost_NilUsage(t *testing.T) {
	got := metrics.CalculateCost(provcore.Usage{}, metrics.ModelPrices{
		InputUsdPerM: fptr(1.00), OutputUsdPerM: fptr(5.00),
	})
	if got.Total != 0 {
		t.Errorf("nil Usage should produce zero Cost, got Total=%v", got.Total)
	}
}

func assertClose(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-10 {
		t.Errorf("%s: got %v want %v", label, got, want)
	}
}
