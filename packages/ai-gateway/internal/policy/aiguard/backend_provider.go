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
	"encoding/json"
	"fmt"
	"log/slog"

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

	// PriceLookup returns (inputPerMillion, outputPerMillion) USD costs
	// for the classifier model, sourced from the in-memory Models
	// snapshot (Hub-pushed; no per-call DB lookup). When nil or returns
	// (0, 0), cost is left zero and Metadata.CostUsd stays unstamped.
	// Optional — backends without a Models snapshot wire nil and the
	// classify path degrades to "ran but cost not recorded" gracefully.
	PriceLookup func(modelID string) (inputPerM, outputPerM float64)
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
	var chatResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
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
	decoded.Metadata.PromptTokens = chatResp.Usage.PromptTokens
	decoded.Metadata.CompletionTokens = chatResp.Usage.CompletionTokens
	if b.PriceLookup != nil {
		inputPerM, outputPerM := b.PriceLookup(b.ModelID)
		if inputPerM > 0 || outputPerM > 0 {
			decoded.Metadata.CostUsd =
				float64(chatResp.Usage.PromptTokens)*inputPerM/1_000_000.0 +
					float64(chatResp.Usage.CompletionTokens)*outputPerM/1_000_000.0
		}
	}
	return decoded, nil
}
