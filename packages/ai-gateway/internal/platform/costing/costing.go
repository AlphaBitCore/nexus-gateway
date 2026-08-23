// Package costing holds the gateway's single token-to-USD pricing formula and
// the single interpretation of a NULL price column.
//
// It exists because the four-tier cost computation was previously written out
// by hand at each call site. The customer request path had the correct version
// (input / cached-read / cached-write / output billed at their own rates); the
// two internal-operations callers — the smart router's LLM decider and the AI
// Guard classifier backend — had a two-tier version that billed the whole
// prompt-token count at the full input rate. On OpenAI and Gemini
// prompt_tokens INCLUDES the cached subset, and cached tokens bill at 0.25-0.5x,
// so those callers systematically over-estimated internal spend in proportion
// to their own cache hit rate — which for a near-identical router prompt is
// very high. Copying the formula a third time would have reproduced the same
// class of drift, so both the formula and the NULL-fallback rule live here and
// nowhere else.
//
// This package is deliberately a leaf: it imports nothing from the rest of the
// gateway, so the policy layer can price a call without pulling in the store
// package's database driver.
package costing

// Rates carries the four per-million-token prices needed to cost a single
// call. Assembled from the Model row, which is the single source of truth for
// all pricing (the provider_pricing table was retired).
//
// store.CachePricing is an alias of this type, so the proxy's cache
// decomposition, the router, and the AI Guard classifier all read the same
// four numbers through the same struct.
type Rates struct {
	InputUSDPerM      float64
	OutputUSDPerM     float64
	CacheWriteUSDPerM float64
	CacheReadUSDPerM  float64
}

// Tokens is one call's token accounting, in the buckets the wire reports them.
//
// Prompt is the TOTAL input including any cached subset, on every adapter:
// Anthropic reports non-cached input plus two separate cache buckets, but the
// shared normalizer sums them into PromptTokens at codec time, so by the time
// counts reach any pricing code the OpenAI/Gemini convention holds universally.
// EstimateUSD therefore subtracts both cache buckets to recover the uncached
// remainder. A caller that passes only the non-cached share here would
// under-bill by exactly the cached amount.
type Tokens struct {
	Prompt        int64
	Completion    int64
	CacheRead     int64
	CacheCreation int64
}

// RatesFromModel assembles Rates from a Model row's four nullable price
// columns and reports whether the model is priced at all.
//
// ok is false when the input price is NULL: a model with no input price has no
// price row worth reading, and every caller treats that as "ran but cost not
// recorded" rather than as a free call. This is the same condition
// cachelayer.LookupCachePricing returns nil for.
//
// A NULL cache-read or cache-write price is NOT missing pricing — it means the
// vendor charges no discount and no surcharge for cached tokens, so it falls
// back to the input price, making the cache decomposition a no-op that sums
// back to the flat input rate. Keeping this rule in one function is the point
// of the package: the internal-ops callers used to have no cache-price concept
// at all, and giving them a second, subtly different fallback would have made
// two pricing regimes for the same model.
func RatesFromModel(input, output, cachedRead, cachedWrite *float64) (Rates, bool) {
	if input == nil {
		return Rates{}, false
	}
	return Rates{
		InputUSDPerM:      *input,
		OutputUSDPerM:     derefOr(output, 0),
		CacheReadUSDPerM:  derefOr(cachedRead, *input),
		CacheWriteUSDPerM: derefOr(cachedWrite, *input),
	}, true
}

// Priced reports whether these rates would produce a non-zero cost for a
// non-zero token count. A model priced explicitly at 0 is genuinely free and
// is distinct from an unpriced model (RatesFromModel's ok=false); callers use
// this to decide whether stamping a zero cost is a statement or an absence.
func (r Rates) Priced() bool {
	return r.InputUSDPerM > 0 || r.OutputUSDPerM > 0 ||
		r.CacheReadUSDPerM > 0 || r.CacheWriteUSDPerM > 0
}

// EstimateUSD bills each token bucket at its own rate.
//
// The uncached input remainder is Prompt minus both cache buckets, because
// Prompt is the total (see Tokens). Without the subtraction, cached tokens
// would be charged at the full input price AND again at their cache rate.
// The remainder is floored at zero so a provider that reports cache counts
// exceeding its own prompt count produces no negative charge — historically
// the source of negative cost values when two price sources disagreed.
func (r Rates) EstimateUSD(t Tokens) float64 {
	const million = 1_000_000.0
	uncachedInput := t.Prompt - t.CacheRead - t.CacheCreation
	if uncachedInput < 0 {
		uncachedInput = 0
	}
	return float64(uncachedInput)*r.InputUSDPerM/million +
		float64(t.CacheRead)*r.CacheReadUSDPerM/million +
		float64(t.CacheCreation)*r.CacheWriteUSDPerM/million +
		float64(t.Completion)*r.OutputUSDPerM/million
}

func derefOr(v *float64, fallback float64) float64 {
	if v == nil {
		return fallback
	}
	return *v
}
