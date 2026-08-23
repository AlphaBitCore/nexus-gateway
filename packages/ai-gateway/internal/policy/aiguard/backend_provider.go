// Package aiguard — configured-provider backend.
//
// The AdapterBackend is a thin call-time wrapper: it resolves the call
// target through [provtarget.Resolver] per classify, picks the matching
// [provcore.Adapter] from the registry, and invokes it with a canonical
// OpenAI chat-completion body. This keeps every internal LLM caller on
// the same (Resolver + Adapter) path.
package aiguard

import (
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/costing"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// AdapterBackend bypasses RoutingEngine and HookPipeline by calling a
// configured provider directly through the shared adapter stack. The
// ProviderID + ModelID pair identifies the classifier model; everything
// else (BaseURL, APIKey, Extras, provider-model-id) is resolved at
// classify time from the latest provider and credential state.
type AdapterBackend struct {
	Resolver   provtarget.Resolver
	Registry   *provcore.Registry
	ProviderID string
	ModelID    string
	Logger     *slog.Logger

	// PriceLookup returns the four per-million USD rates for the classifier
	// model, sourced from the in-memory Models snapshot (Hub-pushed; no
	// per-call DB lookup). When nil or ok=false, cost is left zero and
	// Metadata.CostUsd stays unstamped. Optional — backends without a Models
	// snapshot wire nil and the classify path degrades to "ran but cost not
	// recorded" gracefully.
	//
	// All four rates, not just input and output: the judge template is fixed,
	// so the classifier's prompt caches heavily and the cached share must bill
	// at the cache rate rather than the full input rate.
	PriceLookup func(modelID string) (costing.Rates, bool)
}

// Call sends prompt to the configured provider via the matching adapter
// and returns the parsed Response. Errors on resolver failure, adapter
// lookup failure, adapter Execute error, non-2xx status, or malformed
// judge output.
func (b *AdapterBackend) Call(ctx context.Context, prompt string) (*Response, error) {
	if b == nil || b.Resolver == nil || b.Registry == nil {
		return nil, fmt.Errorf("aiguard provider: backend not fully wired")
	}

	target, err := b.Resolver.Resolve(ctx, b.ProviderID, b.ModelID, provtarget.ResolveHints{})
	if err != nil {
		return nil, fmt.Errorf("aiguard provider: resolve: %w", err)
	}

	if !target.Format.Valid() {
		return nil, fmt.Errorf("aiguard provider: invalid adapter_type %q on provider %q", target.Format, target.ProviderName)
	}
	adapter, ok := b.Registry.Get(target.Format)
	if !ok {
		return nil, fmt.Errorf("aiguard provider: no adapter for format %q", target.Format)
	}

	body := map[string]any{
		"model":           target.ProviderModelID,
		"messages":        []map[string]any{{"role": "user", "content": prompt}},
		"response_format": map[string]any{"type": "json_object"},
	}
	buf := &bytes.Buffer{}
	if err := json.NewEncoder(buf).Encode(body); err != nil {
		return nil, fmt.Errorf("aiguard provider: marshal: %w", err)
	}
	req := provcore.Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
		Body:       buf.Bytes(),
		Stream:     false,
		Target:     target,
	}
	resp, err := adapter.Execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("aiguard provider: adapter: %w", err)
	}
	if resp == nil {
		return nil, fmt.Errorf("aiguard provider: adapter returned nil")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("aiguard provider: status=%d body=%s", resp.StatusCode, string(resp.Body))
	}
	// Only the judge content is parsed here. Token counts come from resp.Usage
	// below — the adapter has already decoded them through the provider's own
	// usage alias chain, so re-reading `usage` from the body would be a second,
	// poorer decoder. The struct this replaced held exactly three token fields
	// and no cache buckets, which is how the classifier came to bill its
	// heavily-cached judge prompt at the full input rate.
	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp.Body, &chatResp); err != nil {
		return nil, fmt.Errorf("aiguard provider: parse: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("aiguard provider: empty choices")
	}
	decoded, err := DecodeJudgeOutput(chatResp.Choices[0].Message.Content)
	if err != nil {
		return nil, err
	}

	// Stamp usage + cost so the sink can persist them on the
	// traffic_event row. Adapters that strip usage from chat completions
	// will leave these zero — the sink treats zero as "unknown" and
	// stores SQL NULL, matching the embedding_cost_usd contract.
	decoded.Metadata.PromptTokens = derefInt(resp.Usage.PromptTokens)
	decoded.Metadata.CompletionTokens = derefInt(resp.Usage.CompletionTokens)
	decoded.Metadata.CacheReadTokens = derefInt(resp.Usage.CacheReadTokens)
	decoded.Metadata.CacheCreationTokens = derefInt(resp.Usage.CacheCreationTokens)
	// Stamp the provider that actually served this call, sourced from the
	// resolved call target (never inferred from ModelID or a string) — this
	// is what lets the sink attribute the classifier's cost to a real
	// provider in the traffic_event rollup instead of leaving it un-routed.
	decoded.Metadata.ProviderID = target.ProviderID
	decoded.Metadata.ProviderName = target.ProviderName
	if b.PriceLookup != nil {
		if rates, priced := b.PriceLookup(b.ModelID); priced {
			decoded.Metadata.CostUsd = rates.EstimateUSD(costing.Tokens{
				Prompt:        int64(decoded.Metadata.PromptTokens),
				Completion:    int64(decoded.Metadata.CompletionTokens),
				CacheRead:     int64(decoded.Metadata.CacheReadTokens),
				CacheCreation: int64(decoded.Metadata.CacheCreationTokens),
			})
		}
	}
	return decoded, nil
}

// derefInt reads a normalizer token count, whose fields are pointers so that
// "the provider reported zero" stays distinguishable from "the provider
// reported nothing". Both collapse to 0 here: Metadata carries plain ints, and
// the sink already treats a zero count as unknown and stores SQL NULL.
func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
