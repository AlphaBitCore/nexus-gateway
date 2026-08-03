package strategies

import (
	"context"
	"fmt"
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

// SmartStrategy calls a router LLM to select the best model from
// available candidates.
type SmartStrategy struct {
	deps SmartDeps
}

func (s *SmartStrategy) Type() string { return "smart" }

func (s *SmartStrategy) Evaluate(ctx context.Context, node core.StrategyNode, rctx *core.RoutingContext, trace *[]core.TraceEntry, _ int, _ RecurseFunc) ([]core.RoutingTarget, error) {
	start := time.Now()

	// model=auto on a non-chat endpoint: the LLM task-router (token sizing,
	// task classification, tool/vision capability) is chat-specific and has no
	// meaning for image / audio / video / rerank generation. Pick
	// deterministically among that endpoint modality's enabled models instead
	// — health/latency ranking downstream orders them — so auto is
	// modality-aware and never crosses into a chat model.
	if !endpointUsesLLMRouter(rctx.EndpointType) {
		return s.modalityAutoTargets(ctx, rctx, trace, start)
	}

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

	// Resolve the router model's own declared context window from the
	// raw (pre-VK-filter) rows — the router model routes traffic, it is
	// not itself a routing candidate, so the VK allowlist must not hide
	// it. 0 (not found / undeclared) lets the prompt builder fall back
	// to its conservative built-in window.
	routerCtxLimit := 0
	for _, c := range candidates {
		if c.ProviderID == cfg.RouterProviderID && (c.ModelID == cfg.RouterModelID || c.ModelCode == cfg.RouterModelID) {
			if c.MaxContextTokens != nil {
				routerCtxLimit = *c.MaxContextTokens
			}
			break
		}
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

	estTokens, estImages := 0, 0
	if rctx.Request != nil && rctx.Request.Kind.IsAI() {
		estTokens, estImages = estimateRequestTokens(rctx.Request)

		// Capability hard filter FIRST: capability is a definite binary
		// constraint (a non-vision model genuinely cannot serve images),
		// whereas the context filter below is a coarse size estimate. A
		// candidate that declares features but lacks what the request
		// provably needs (vision for image blocks, function_calling for
		// tool definitions) is removed before the router sees the
		// catalog. Undeclared feature lists pass, and a dimension that
		// would empty the pool is skipped (both fail-open). Running this
		// before the size filter means a capable-but-maybe-tight model
		// survives to the context filter's keep-largest fallback (whose
		// overflow risk the executor's context-overflow failover catches)
		// rather than being size-dropped while a blind model is kept.
		capKept, capDropped, skippedDims := filterByCapability(candidates, estImages > 0, len(rctx.Request.Tools) > 0)
		candidates = capKept
		if capDropped > 0 {
			*trace = append(*trace, core.TraceEntry{
				StrategyType: "smart",
				Decision:     fmt.Sprintf("capability filter: dropped %d/%d candidates lacking required features", capDropped, capDropped+len(capKept)),
				DurationMs:   int(time.Since(start).Milliseconds()),
			})
		}
		for _, dim := range skippedDims {
			*trace = append(*trace, core.TraceEntry{
				StrategyType: "smart",
				Decision:     fmt.Sprintf("capability filter: no candidate declares %q; dimension skipped (fail-open)", dim),
				DurationMs:   int(time.Since(start).Milliseconds()),
			})
		}

		// Hard context-window filter: drop candidates whose declared
		// context window provably cannot hold this request. The catalog's
		// mx value is only a hint to the router LLM — and the router is
		// shown only a bounded slice of the recent conversation, so it
		// cannot judge the true request size (tool and assistant history
		// usually dominate). The filter runs here, on the full canonical
		// payload, so an undersized model never even appears in the
		// catalog.
		reserve := defaultOutputReserveTokens
		if rctx.Request.Params != nil && rctx.Request.Params.MaxTokens != nil && *rctx.Request.Params.MaxTokens > 0 {
			reserve = *rctx.Request.Params.MaxTokens
		}
		required := estTokens + reserve
		kept, dropped, noneFit := filterByContextWindow(candidates, required)
		candidates = kept
		if noneFit {
			*trace = append(*trace, core.TraceEntry{
				StrategyType: "smart",
				Decision:     fmt.Sprintf("context filter: no candidate fits est. %d tokens; keeping largest-context candidates (overflow risk)", required),
				DurationMs:   int(time.Since(start).Milliseconds()),
			})
		} else if dropped > 0 {
			*trace = append(*trace, core.TraceEntry{
				StrategyType: "smart",
				Decision:     fmt.Sprintf("context filter: dropped %d/%d candidates below est. %d required tokens", dropped, dropped+len(kept), required),
				DurationMs:   int(time.Since(start).Milliseconds()),
			})
		}
	}

	// 2. Build model catalog JSON.
	catalog := buildModelCatalog(candidates)

	// 3. Prepare the router-LLM system prompt with the catalog substituted.
	systemPrompt := cfg.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = llm.DefaultSystemPrompt
	}
	systemPrompt = strings.Replace(systemPrompt, "{modelCatalog}", catalog, 1)

	// 4. Project the canonical request to the router conversation:
	// user + assistant turns, in order. Client system messages are
	// excluded — they are large (agent frameworks) and low-signal for
	// routing; the metadata line below carries the request shape
	// instead. Tool-role plumbing is excluded the same way. Two
	// negative-case short-circuits keep the router LLM from being
	// called with empty or non-AI content — eliminating wasted upstream
	// cost and codec-level rejections on Anthropic-shape routers.
	if rctx.Request == nil || !rctx.Request.Kind.IsAI() {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "smart",
			Decision:     "request payload not normalizable for smart routing; using default",
			DurationMs:   int(time.Since(start).Milliseconds()),
		})
		return smartFallback(ctx, cfg, s.deps, trace, start)
	}
	var convMsgs []normcore.Message
	hasUser := false
	for _, m := range rctx.Request.Messages {
		if m.Role == normcore.RoleUser || m.Role == normcore.RoleAssistant {
			convMsgs = append(convMsgs, m)
			if m.Role == normcore.RoleUser {
				hasUser = true
			}
		}
	}
	if !hasUser {
		*trace = append(*trace, core.TraceEntry{
			StrategyType: "smart",
			Decision:     "smart routing: no user content in request; using default",
			DurationMs:   int(time.Since(start).Milliseconds()),
		})
		return smartFallback(ctx, cfg, s.deps, trace, start)
	}

	// Request-shape metadata for the router: it sees only a bounded
	// text slice of the conversation, so the counts it needs for
	// capability matching (vision, function calling, long context —
	// rule 2 of the default prompt) ride in one cheap line.
	systemPrompt += fmt.Sprintf(
		"\n\n## Request Metadata\nIncoming request: ~%d input tokens, %d images, %d tool definitions.",
		estTokens, estImages, len(rctx.Request.Tools),
	)

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
		SystemPrompt:       systemPrompt,
		Messages:           convMsgs,
		RouterContextLimit: routerCtxLimit,
		Temperature:        cfg.temperature(),
		MaxTokens:          cfg.maxTokens(),
		Timeout:            time.Duration(cfg.timeoutMs()) * time.Millisecond,
		RouterProviderID:   cfg.RouterProviderID,
		RouterModelID:      cfg.RouterModelID,
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

	return s.armContextUpgrade(ctx, candidates, selected, target, trace, start), nil
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
