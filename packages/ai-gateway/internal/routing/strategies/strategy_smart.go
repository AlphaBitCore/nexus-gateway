package strategies

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/llm"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// SmartConfig holds the configuration for the smart routing strategy.
// It lives inside core.StrategyNode.Smart and specifies the router LLM
// that analyzes user prompts to select the best model.
type SmartConfig struct {
	RouterProviderID  string   `json:"routerProviderId"`
	RouterModelID     string   `json:"routerModelId"`
	SystemPrompt      string   `json:"systemPrompt,omitempty"`
	Temperature       *float64 `json:"temperature,omitempty"`
	MaxTokens         int      `json:"maxTokens,omitempty"`
	TimeoutMs         int      `json:"timeoutMs,omitempty"`
	DefaultProviderID string   `json:"defaultProviderId,omitempty"`
	DefaultModelID    string   `json:"defaultModelId,omitempty"`
}

func (c *SmartConfig) temperature() float64 {
	if c.Temperature != nil {
		return *c.Temperature
	}
	return 0
}

func (c *SmartConfig) maxTokens() int {
	if c.MaxTokens > 0 {
		return c.MaxTokens
	}
	return 1024
}

// timeoutMs is the router-LLM call budget. The built-in default is 3000ms:
// smart routing sits on the request hot path, so a slow router provider must
// not stall every routed request for long before smartFallback kicks in.
// Operators override per-rule via SmartConfig.TimeoutMs when a slower router
// model genuinely needs more headroom.
func (c *SmartConfig) timeoutMs() int {
	if c.TimeoutMs > 0 {
		return c.TimeoutMs
	}
	return 3000
}

// smartCatalogProvider is one provider group in the router system prompt JSON.
// Keys are intentionally short to reduce tokens; see llm.DefaultSystemPrompt.
type smartCatalogProvider struct {
	ProviderID string            `json:"p"`
	Models     []smartCatalogRow `json:"m"`
}

// smartCatalogRow is one routable model. JSON key i is Model.code only
// (the UUID Model.id and providerModelId are intentionally omitted from
// the catalog JSON — the router LLM is shown the short customer-facing code).
// ip/op USD per 1M tokens, f = feature tags, mx/mo = context and output limits.
type smartCatalogRow struct {
	ID       string   `json:"i"`
	InPM     *float64 `json:"ip,omitempty"`
	OutPM    *float64 `json:"op,omitempty"`
	Features []string `json:"f,omitempty"`
	MaxCtx   *int     `json:"mx,omitempty"`
	MaxOut   *int     `json:"mo,omitempty"`
}

// SmartStrategy calls a router LLM to select the best model from
// available candidates.
type SmartStrategy struct {
	deps SmartDeps
}

func (s *SmartStrategy) Type() string { return "smart" }

func (s *SmartStrategy) Evaluate(ctx context.Context, node core.StrategyNode, rctx *core.RoutingContext, trace *[]core.TraceEntry, _ int, _ RecurseFunc) ([]core.RoutingTarget, error) {
	start := time.Now()
	if node.RouterProviderID == "" || node.RouterModelID == "" {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "smart",
			Decision:     "missing smart config on node",
			DurationMs:   int(time.Since(start).Milliseconds()),
		})
		return nil, nil
	}
	cfg := &SmartConfig{
		RouterProviderID:  node.RouterProviderID,
		RouterModelID:     node.RouterModelID,
		SystemPrompt:      node.SystemPrompt,
		Temperature:       node.Temperature,
		MaxTokens:         node.MaxTokens,
		TimeoutMs:         node.TimeoutMs,
		DefaultProviderID: node.DefaultProviderID,
		DefaultModelID:    node.DefaultModelID,
	}

	// 1. Get candidate models.
	candidates, err := s.deps.Store.ListEnabledChatModels(ctx)
	if err != nil {
		s.deps.Logger.Warn("smart: list candidates failed", "error", err)
		return smartFallback(ctx, cfg, s.deps, trace, start)
	}

	// Filter by VK allowed models if present.
	if rctx.VirtualKey != nil && len(rctx.VirtualKey.AllowedModels) > 0 {
		var filtered []core.SmartModelRow
		for _, c := range candidates {
			if core.ModelMatchesAllowedRefs(c.ModelID, c.ProviderModelID, c.ProviderID, rctx.VirtualKey.AllowedModels) {
				filtered = append(filtered, c)
			}
		}
		candidates = filtered
	}

	if len(candidates) == 0 {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "smart",
			Decision:     "no candidate models available",
			DurationMs:   int(time.Since(start).Milliseconds()),
		})
		return smartFallback(ctx, cfg, s.deps, trace, start)
	}

	// 2. Build model catalog JSON.
	catalog := buildModelCatalog(candidates)

	// 3. Prepare the router-LLM system prompt with the catalog substituted.
	systemPrompt := cfg.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = llm.DefaultSystemPrompt
	}
	systemPrompt = strings.Replace(systemPrompt, "{modelCatalog}", catalog, 1)

	// 4. Filter the canonical request payload for role=user content.
	// The smart router LLM picks a model based on what the end user
	// asked for, so system / assistant / tool roles are dropped here.
	// Two negative-case short-circuits immediately follow the filter so
	// the router LLM is never called with empty or non-AI content —
	// eliminating wasted upstream cost and codec-level rejections on
	// Anthropic-shape routers.
	if rctx.Request == nil || !rctx.Request.Kind.IsAI() {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "smart",
			Decision:     "request payload not normalizable for smart routing; using default",
			DurationMs:   int(time.Since(start).Milliseconds()),
		})
		return smartFallback(ctx, cfg, s.deps, trace, start)
	}
	var userMsgs []normcore.Message
	for _, m := range rctx.Request.Messages {
		if m.Role == normcore.RoleUser {
			userMsgs = append(userMsgs, m)
		}
	}
	if len(userMsgs) == 0 {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "smart",
			Decision:     "smart routing: no user content in request; using default",
			DurationMs:   int(time.Since(start).Milliseconds()),
		})
		return smartFallback(ctx, cfg, s.deps, trace, start)
	}

	// 5. Hand off to the Decider (llm.Decider).
	if s.deps.RouterLLM == nil {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "smart",
			Decision:     "router LLM client not wired",
			DurationMs:   int(time.Since(start).Milliseconds()),
		})
		return smartFallback(ctx, cfg, s.deps, trace, start)
	}
	decision, err := s.deps.RouterLLM.Decide(ctx, llm.Request{
		SystemPrompt:     systemPrompt,
		UserMessages:     userMsgs,
		Temperature:      cfg.temperature(),
		MaxTokens:        cfg.maxTokens(),
		Timeout:          time.Duration(cfg.timeoutMs()) * time.Millisecond,
		RouterProviderID: cfg.RouterProviderID,
		RouterModelID:    cfg.RouterModelID,
	})
	if err != nil {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "smart",
			Decision:     err.Error(),
			DurationMs:   int(time.Since(start).Milliseconds()),
		})
		return smartFallback(ctx, cfg, s.deps, trace, start)
	}

	// 6. Resolve router-selected model token to an internal model ID.
	modelID, routerProviderID, reason := decision.ModelID, decision.ProviderID, decision.Reason
	selectedID, ok := resolveSelectedModelID(modelID, routerProviderID, candidates)
	if !ok {
		s.deps.Logger.Warn("smart: router returned unknown model", "modelId", modelID, "providerId", routerProviderID)
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "smart",
			Decision:     fmt.Sprintf("router returned unknown model %q", modelID),
			DurationMs:   int(time.Since(start).Milliseconds()),
		})
		return smartFallback(ctx, cfg, s.deps, trace, start)
	}

	// 8. Find the candidate and resolve the target.
	var selected *core.SmartModelRow
	for i := range candidates {
		if candidates[i].ModelID == selectedID {
			selected = &candidates[i]
			break
		}
	}

	target, err := s.deps.Lookup(ctx, selected.ProviderID, selected.ModelID)
	if err != nil {
		s.deps.Logger.Warn("smart: target lookup failed", "modelId", selectedID, "error", err)
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "smart",
			Decision:     fmt.Sprintf("target lookup failed for %q: %v", selectedID, err),
			DurationMs:   int(time.Since(start).Milliseconds()),
		})
		return smartFallback(ctx, cfg, s.deps, trace, start)
	}

	durationMs := int(time.Since(start).Milliseconds())
	*trace = append(*trace, core.TraceEntry{
		StrategyType: "smart",
		Decision:     fmt.Sprintf("selected %s [%s/%s] — %s", core.FormatTargetFriendly(target), selected.ProviderID, selected.ModelID, reason),
		DurationMs:   durationMs,
	})

	return []core.RoutingTarget{*target}, nil
}

// resolveSelectedModelID maps the router-returned token to an internal model
// UUID. The LLM is shown ModelCode entries in the catalog (short, recognisable
// strings like "gpt-4o"), and most accurate returns are codes. We also accept
// a UUID match (in case admin reuses the prompt with the internal id) and a
// unique providerModelId match (DeepSeek-style "deepseek-chat" coincides with
// code on the seed-shipped catalog but may diverge later). Returns the UUID
// suitable for [core.TargetLookup] and FK references.
//
// When providerID is non-empty, matching is restricted to that provider's rows
// so an LLM that returned an ambiguous code under the wrong provider doesn't
// silently land on a different vendor.
func resolveSelectedModelID(token, providerID string, candidates []core.SmartModelRow) (string, bool) {
	if providerID != "" {
		var narrowed []core.SmartModelRow
		for _, c := range candidates {
			if c.ProviderID == providerID {
				narrowed = append(narrowed, c)
			}
		}
		if len(narrowed) == 0 {
			return "", false
		}
		candidates = narrowed
	}
	// Code match (the catalog's `i` field) — the canonical LLM happy path.
	for _, c := range candidates {
		if c.ModelCode == token {
			return c.ModelID, true
		}
	}
	// UUID match — accept if admin's prompt happens to reference the
	// internal id directly.
	for _, c := range candidates {
		if c.ModelID == token {
			return c.ModelID, true
		}
	}
	// providerModelId — best-effort fallback for LLM outputs that lifted
	// the upstream vendor name verbatim. Only accept a unique match.
	var matchedID string
	matches := 0
	for _, c := range candidates {
		if c.ProviderModelID == token {
			matchedID = c.ModelID
			matches++
		}
	}
	if matches == 1 {
		return matchedID, true
	}
	return "", false
}

// smartFallback resolves the default model from SmartConfig, or returns empty.
func smartFallback(ctx context.Context, cfg *SmartConfig, deps SmartDeps, trace *[]core.TraceEntry, start time.Time) ([]core.RoutingTarget, error) {
	if cfg.DefaultProviderID == "" || cfg.DefaultModelID == "" {
		return nil, nil
	}
	target, err := deps.Lookup(ctx, cfg.DefaultProviderID, cfg.DefaultModelID)
	if err != nil {
		return nil, nil //nolint:nilerr // smart fallback is best-effort; missing default is not fatal
	}
	*trace = append(*trace, core.TraceEntry{
		StrategyType: "smart",
		Decision:     fmt.Sprintf("falling back to default %s [%s/%s]", core.FormatTargetFriendly(target), cfg.DefaultProviderID, cfg.DefaultModelID),
		DurationMs:   int(time.Since(start).Milliseconds()),
	})
	return []core.RoutingTarget{*target}, nil
}

// buildModelCatalog converts candidate rows into compact JSON for the system
// prompt: providers with nested models (i = Model.code, optional pricing and
// limits). We send the customer-facing code rather than the UUID so the LLM
// has a recognisable, short token to return; resolveSelectedModelID maps it
// back to a UUID for the runtime lookup.
func buildModelCatalog(candidates []core.SmartModelRow) string {
	order := make([]string, 0)
	seen := make(map[string]struct{})
	byProvider := make(map[string][]smartCatalogRow)
	for _, c := range candidates {
		if _, ok := seen[c.ProviderID]; !ok {
			seen[c.ProviderID] = struct{}{}
			order = append(order, c.ProviderID)
		}
		row := smartCatalogRow{ID: c.ModelCode}
		if c.InputPricePM != nil {
			row.InPM = c.InputPricePM
		}
		if c.OutputPricePM != nil {
			row.OutPM = c.OutputPricePM
		}
		if len(c.Features) > 0 {
			row.Features = c.Features
		}
		if c.MaxContextTokens != nil {
			row.MaxCtx = c.MaxContextTokens
		}
		if c.MaxOutputTokens != nil {
			row.MaxOut = c.MaxOutputTokens
		}
		byProvider[c.ProviderID] = append(byProvider[c.ProviderID], row)
	}
	out := make([]smartCatalogProvider, 0, len(order))
	for _, pid := range order {
		out = append(out, smartCatalogProvider{
			ProviderID: pid,
			Models:     byProvider[pid],
		})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b)
}
