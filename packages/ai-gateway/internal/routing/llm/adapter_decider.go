package llm

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/costing"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// AdapterLookup resolves a wire-format key to a registered provider
// adapter. Defined as an interface here so AdapterDecider does not depend
// on the concrete *provcore.Registry type; *provcore.Registry satisfies
// the contract.
type AdapterLookup interface {
	Get(format provcore.Format) (provcore.Adapter, bool)
}

// AdapterDecider is the production Decider implementation. It resolves
// the router LLM's CallTarget via a provtarget.Resolver, picks the
// matching provider Adapter, builds the canonical OpenAI request body,
// calls the upstream, and parses the response.
type AdapterDecider struct {
	resolver provtarget.Resolver
	adapters AdapterLookup
	logger   *slog.Logger

	// PriceLookup returns the four per-million USD rates for the router model.
	// Nil, or ok=false, leaves CostUsd zero — attribution is still recorded.
	// Mirrors aiguard.AdapterBackend.PriceLookup.
	//
	// All four rates, not just input and output: the router prompt is the same
	// system prompt plus the same model catalog on nearly every call, so the
	// provider caches almost all of it. Pricing that prompt off the input rate
	// alone charged the cached majority at full price.
	PriceLookup func(modelID string) (costing.Rates, bool)
}

// NewAdapterDecider constructs the production Decider.
func NewAdapterDecider(resolver provtarget.Resolver, adapters AdapterLookup, logger *slog.Logger) *AdapterDecider {
	return &AdapterDecider{
		resolver: resolver,
		adapters: adapters,
		logger:   logger,
	}
}

// Decide runs the full router-LLM call pipeline. Error text must remain
// byte-identical to the audit routing_trace vocabulary for the same
// failure modes.
func (a *AdapterDecider) Decide(ctx context.Context, req Request) (Decision, error) {
	target, err := a.resolver.Resolve(ctx, req.RouterProviderID, req.RouterModelID, provtarget.ResolveHints{})
	if err != nil {
		a.logger.Warn("smart: router target resolve failed", "error", err)
		return Decision{}, fmt.Errorf("router target resolve failed: %w", err)
	}
	if !target.Format.Valid() {
		return Decision{}, fmt.Errorf("invalid adapter_type on router provider %q (%q)", target.ProviderName, target.Format)
	}
	adapter, ok := a.adapters.Get(target.Format)
	if !ok {
		return Decision{}, fmt.Errorf("no adapter for router provider %q (format %q)", target.ProviderName, target.Format)
	}

	body := BuildRequestBody(target.ProviderModelID, req)
	bodyBytes, _ := json.Marshal(body)

	callCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	resp, err := adapter.Execute(callCtx, provcore.Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
		Body:       bodyBytes,
		Stream:     false,
		Target:     target,
	})
	if err != nil {
		isTimeout := callCtx.Err() != nil
		a.logger.Warn("smart: router LLM call failed", "error", err, "timeout", isTimeout)
		if isTimeout {
			return Decision{}, fmt.Errorf("router LLM timeout (%dms)", req.Timeout.Milliseconds())
		}
		return Decision{}, fmt.Errorf("router LLM error: %w", err)
	}
	if resp.StatusCode >= 400 {
		a.logger.Warn("smart: router LLM returned error", "status", resp.StatusCode, "provider", target.ProviderName)
		return Decision{}, fmt.Errorf("router LLM error: %d", resp.StatusCode)
	}

	d, err := ParseResponse(string(resp.Body))
	if err != nil {
		a.logger.Warn("smart: failed to parse router response", "error", err)
		return Decision{}, fmt.Errorf("failed to parse router response")
	}

	// The router call is real vendor spend on the provider that served it.
	// Recorded even when unpriced: attribution without an amount still lets
	// the reconciliation report show which vendor was charged.
	//
	// Usage comes from resp.Usage — the counts the adapter already decoded and
	// ran through the provider's own alias chain — not from a local re-parse of
	// the body. A hand-rolled struct here read only prompt_tokens and
	// completion_tokens, so the cache buckets the adapter had already recovered
	// were dropped on the floor and the whole prompt was then billed at the full
	// input rate. Since the router's prompt is near-identical call to call and
	// caches at a high rate, that over-estimated router spend in proportion to
	// routing volume. Missing counts stay zero rather than failing a routing
	// decision that already succeeded.
	d.PromptTokens = derefInt(resp.Usage.PromptTokens)
	d.CompletionTokens = derefInt(resp.Usage.CompletionTokens)
	d.CacheReadTokens = derefInt(resp.Usage.CacheReadTokens)
	d.CacheCreationTokens = derefInt(resp.Usage.CacheCreationTokens)
	// target.ProviderID is the ONLY source for this field: it is the provider
	// that served the router call, never the provider of the model the router
	// picked (d.ProviderID).
	d.ServedProviderID = target.ProviderID
	if a.PriceLookup != nil {
		if rates, priced := a.PriceLookup(req.RouterModelID); priced {
			d.CostUsd = rates.EstimateUSD(costing.Tokens{
				Prompt:        int64(d.PromptTokens),
				Completion:    int64(d.CompletionTokens),
				CacheRead:     int64(d.CacheReadTokens),
				CacheCreation: int64(d.CacheCreationTokens),
			})
		}
	}
	return d, nil
}

// derefInt reads a normalizer token count, whose fields are pointers so that
// "the provider reported zero" stays distinguishable from "the provider
// reported nothing". Both collapse to 0 here: Decision carries plain ints, and
// a router call with no usage block is billed as no tokens either way.
func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
