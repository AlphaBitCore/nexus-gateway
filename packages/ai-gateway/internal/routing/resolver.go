package routing

import (
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/capability"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/matcher"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/strategies"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// routingStore is the narrow DB contract the Resolver depends on.
// *store.DB satisfies it. Declared as an interface so tests can inject
// a fake without a live Postgres, covering the full pipeline end-to-end.
type routingStore interface {
	GetEnabledRoutingRules(ctx context.Context) ([]store.RoutingRule, error)
	GetProviderAndModel(ctx context.Context, providerID, modelID string) (*store.Provider, *store.Model, error)
	GetModel(ctx context.Context, id string) (*store.Model, error)
	// ResolveModelCandidates resolves a customer-facing request string
	// (Model.code or an entry in Model.aliases) to every enabled Model
	// row that responds to it. See store.DB.ResolveModelCandidates.
	ResolveModelCandidates(ctx context.Context, code string) ([]store.Model, error)
}

// Resolver is the main route resolution engine. It orchestrates the
// stage-0 (policy narrowing) → stage-1 (route decision) pipeline.
type Resolver struct {
	db              routingStore
	registry        *strategies.StrategyRegistry
	logger          *slog.Logger
	narrowingEngine *matcher.NarrowingEngine
	healthRanker    *core.HealthRanker
	// capCache is the atomically swappable capability snapshot used by the
	// embeddings pre-filter. Nil disables the pre-filter so callers
	// that do not wire a capability cache (tests, degraded paths) are not
	// affected.
	capCache *capability.Cache
	// condCache memoizes parsed rule MatchConditions, content-addressed by a
	// hash of the raw JSON. ruleMatches runs per-rule on every request (across
	// stage-0 narrowing and the stage-1 loop), so without this each request
	// re-unmarshals every rule's conditions. Self-invalidating: a changed rule
	// has new bytes → a new key. Lock-free; see parseMatchConditions.
	condCache sync.Map // map[uint64]*condCacheEntry
	// contentCache memoizes, per rule Config (StrategyNode tree), whether the
	// tree contains a content-reading strategy (e.g. smart). RequestNeedsCanonical
	// runs per request to drive the lazy-canonical lazy-canonical gate; without this it
	// would re-unmarshal every rule's Config each request. Self-invalidating like
	// condCache (changed Config -> new bytes -> new key). Lock-free.
	contentCache sync.Map // map[uint64]*contentCacheEntry
}

// condCacheEntry is a parsed-conditions cache value. raw is retained so a hash
// hit can be byte-verified, ruling out the rare FNV-64 collision. conds is nil
// when the raw JSON is malformed (cached so a bad rule is not re-parsed).
type condCacheEntry struct {
	raw   []byte
	conds *core.MatchConditions
}

// parseMatchConditions returns the parsed MatchConditions for raw, unmarshaling
// once and serving the cached result thereafter. Returns nil for malformed
// JSON (callers treat that as "no match", preserving the prior behavior). The
// distinct-conditions set equals the number of routing rules — tiny — so the
// map needs no eviction.
func (r *Resolver) parseMatchConditions(raw json.RawMessage) *core.MatchConditions {
	h := fnv.New64a()
	_, _ = h.Write(raw)
	key := h.Sum64()
	if v, ok := r.condCache.Load(key); ok {
		if e := v.(*condCacheEntry); bytes.Equal(e.raw, raw) {
			return e.conds
		}
	}
	var conds core.MatchConditions
	var parsed *core.MatchConditions
	if err := json.Unmarshal(raw, &conds); err == nil {
		parsed = &conds
	}
	r.condCache.Store(key, &condCacheEntry{raw: append([]byte(nil), raw...), conds: parsed})
	return parsed
}

// NewResolver creates a Resolver with the given catalog source and
// strategy registry. The catalog source is normally *cachelayer.Layer
// (in-memory snapshots for Provider/Model + delegation to the bespoke
// rulesCache for routing rules); *store.DB also satisfies the
// interface for tests and degraded paths.
//
// capCache may be nil — the capability pre-filter is skipped when nil.
func NewResolver(catalog routingStore, registry *strategies.StrategyRegistry, healthRanker *core.HealthRanker, logger *slog.Logger, capCache *capability.Cache) *Resolver {
	return &Resolver{
		db:              catalog,
		registry:        registry,
		logger:          logger,
		narrowingEngine: &matcher.NarrowingEngine{},
		healthRanker:    healthRanker,
		capCache:        capCache,
	}
}

// Resolve runs the full routing pipeline and returns a RoutingPlan.
func (r *Resolver) Resolve(ctx context.Context, rctx *core.RoutingContext) (*core.RoutingPlan, error) {
	r.hydrateRequestedModel(ctx, rctx)
	plan := &core.RoutingPlan{
		OriginalModelID: rctx.RequestedModel.ID,
	}

	// Fetch all enabled routing rules (cached).
	allRules, err := r.db.GetEnabledRoutingRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("router: fetch rules: %w", err)
	}

	// --- Stage 0: Policy narrowing ---
	stage0Start := time.Now()

	narrowing, _ := r.narrowingEngine.Apply(ctx, allRules, rctx, r.logger, r.ruleMatches)

	if !matcher.IsNarrowingEmpty(narrowing) {
		summary := matcher.ToSummary(narrowing)
		plan.NarrowingSummary = &summary
	}

	plan.PipelineTrace = append(plan.PipelineTrace, core.PipelineTraceEntry{
		Stage:      0,
		Decision:   "policy narrowing applied",
		DurationMs: int(time.Since(stage0Start).Milliseconds()),
	})

	// --- Stage 1: Route decision ---
	stage1Start := time.Now()

	var primaryRule *store.RoutingRule
	var fallbackRules []store.RoutingRule

	for i := range allRules {
		rule := &allRules[i]
		if rule.PipelineStage != 1 {
			continue
		}
		if !r.ruleMatches(*rule, rctx.RequestedModel.ID, rctx) {
			continue
		}
		if rule.StrategyType == "fallback" {
			fallbackRules = append(fallbackRules, *rule)
		} else if primaryRule == nil {
			primaryRule = rule
		}
	}

	if primaryRule != nil {
		plan.RuleID = primaryRule.ID
		plan.RuleName = primaryRule.Name
		// Carry the primary rule's per-rule RetryPolicy JSON forward.
		// The handler field-merges it on top of the YAML default before
		// invoking the executor. Fallback rules' retry policies are
		// intentionally ignored — only the primary rule's policy
		// determines L2/L3 behavior for the routed targets.
		plan.RuleRetryPolicyJSON = primaryRule.RetryPolicy

		var node core.StrategyNode
		if err := json.Unmarshal(primaryRule.Config, &node); err != nil {
			return nil, fmt.Errorf("router: parse strategy config: %w", err)
		}

		targets, err := r.registry.Evaluate(ctx, node, rctx, &plan.Trace, 0)
		if err != nil {
			r.logger.Warn("strategy evaluation failed", "ruleId", primaryRule.ID, "error", err)
		} else {
			// Filter targets by narrowing + VK allowed models.
			filtered := r.narrowingEngine.Filter(targets, narrowing, rctx)
			for i := range filtered {
				filtered[i].Source = "primary"
			}
			plan.Targets = filtered
		}

		// Recovery targets from inline fallback chain.
		if len(primaryRule.FallbackChain) > 0 {
			var chain []core.FallbackChainEntry
			// best-effort: a malformed fallback chain just yields no recovery
			// targets; the primary plan still routes normally.
			_ = json.Unmarshal(primaryRule.FallbackChain, &chain)
			var fbTargets []core.RoutingTarget
			for _, entry := range chain {
				target, err := r.lookupTarget(ctx, entry.ProviderID, entry.ModelID)
				if err == nil {
					fbTargets = append(fbTargets, *target)
				}
			}
			// Inline fallback-chain recovery targets MUST satisfy the
			// VK's allowedModels allowlist, exactly like the primary (above) and
			// fallbackRules (below) paths. Without this Filter a FallbackChain
			// entry pointing at a provider/model outside the VK's allowlist would
			// be dispatched — and its upstream credential consumed — on primary
			// failure, silently escaping the per-VK model-scope boundary.
			filtered := r.narrowingEngine.Filter(fbTargets, narrowing, rctx)
			for i := range filtered {
				filtered[i].Source = "fallback"
			}
			plan.RecoveryTargets = append(plan.RecoveryTargets, filtered...)
		}
	}

	// Recovery from fallback rules.
	for _, rule := range fallbackRules {
		var node core.StrategyNode
		if err := json.Unmarshal(rule.Config, &node); err != nil {
			continue
		}
		targets, err := r.registry.Evaluate(ctx, node, rctx, &plan.Trace, 0)
		if err != nil {
			continue
		}
		recoveryTargets := r.narrowingEngine.Filter(targets, narrowing, rctx)
		for i := range recoveryTargets {
			recoveryTargets[i].Source = "recovery"
		}
		plan.RecoveryTargets = append(plan.RecoveryTargets, recoveryTargets...)
	}

	// --- Stage 1.5: Capability pre-filter (embeddings endpoint only) ---
	// Applied after narrowingEngine.Filter (stage 1) but before health-aware
	// reorder in ResolveTargets (stage 2). Only fires when:
	//   - the request is an embeddings endpoint
	//   - a capability cache is wired
	//   - the routing context carries an EmbeddingRequest (populated by proxy handler)
	//
	// The filter keeps targets whose Model has an embeddings capability that
	// matches the request parameters; rejects the rest. When every target is
	// rejected, Resolve returns *core.NoCompatibleProviderError with
	// available_capabilities so the handler can surface a rich 400 body.
	//
	// Both PRIMARY (plan.Targets) and recovery/fallback (plan.RecoveryTargets)
	// targets are filtered: ResolveTargets flattens both into allTargets and
	// gates the rich-400 on the combined count, so a recovery target that
	// bypassed the capability check would let an incompatible request dispatch
	// instead of returning the 400.
	if rctx.EndpointType == typology.EndpointKindEmbeddings && r.capCache != nil && rctx.EmbeddingRequest != nil {
		plan.Targets = r.applyCapabilityFilter(plan.Targets, rctx)
		plan.RecoveryTargets = r.applyCapabilityFilter(plan.RecoveryTargets, rctx)
	}

	// Check if model was substituted.
	if len(plan.Targets) > 0 && plan.Targets[0].ModelID != rctx.RequestedModel.ID {
		plan.Substituted = true
	}

	plan.PipelineTrace = append(plan.PipelineTrace, core.PipelineTraceEntry{
		Stage:      1,
		Decision:   fmt.Sprintf("resolved %d targets, %d recovery", len(plan.Targets), len(plan.RecoveryTargets)),
		DurationMs: int(time.Since(stage1Start).Milliseconds()),
	})

	return plan, nil
}

// applyCapabilityFilter runs the capability pre-filter on targets for an
// embeddings request. It returns the kept targets subset and logs each
// rejection at Debug level. When every target is rejected it returns a
// nil slice (the caller checks len(plan.Targets) == 0 downstream).
//
// The function does NOT return *core.NoCompatibleProviderError directly —
// that error is surfaced later, after the fallback-rule recovery pass, in
// ResolveTargets. This keeps the error-surface in one place.
func (r *Resolver) applyCapabilityFilter(targets []core.RoutingTarget, rctx *core.RoutingContext) []core.RoutingTarget {
	snap := r.capCache.Load()
	embReq := &capability.EmbeddingRequest{
		Dimensions:     rctx.EmbeddingRequest.Dimensions,
		BatchSize:      rctx.EmbeddingRequest.BatchSize,
		EncodingFormat: rctx.EmbeddingRequest.EncodingFormat,
		InputType:      rctx.EmbeddingRequest.InputType,
		TaskType:       rctx.EmbeddingRequest.TaskType,
	}
	kept := make([]core.RoutingTarget, 0, len(targets))
	for _, t := range targets {
		cap := snap.Get(t.ModelID)
		ok, reason, _ := capability.Compatible(embReq, cap)
		if ok {
			kept = append(kept, t)
		} else {
			r.logger.Debug("capability pre-filter rejected target",
				"modelID", t.ModelID,
				"modelCode", t.ModelCode,
				"reason", reason,
			)
		}
	}
	return kept
}

// ruleMatches checks if a routing rule applies to the current context. A rule
// with empty matchConditions matches every request — that is the semantic
// contract for a catch-all rule. Per-rule model filtering lives exclusively
// in matchConditions.models.
func (r *Resolver) ruleMatches(rule store.RoutingRule, modelID string, rctx *core.RoutingContext) bool {
	if len(rule.MatchConditions) > 0 {
		conds := r.parseMatchConditions(rule.MatchConditions)
		if conds == nil {
			// Malformed conditions never match — same as the prior
			// unmarshal-error path.
			return false
		}
		return matcher.RuleMatchesContext(conds, modelID, rctx)
	}
	return true
}

// lookupTarget resolves a provider+model pair into a RoutingTarget.
func (r *Resolver) lookupTarget(ctx context.Context, providerID, modelID string) (*core.RoutingTarget, error) {
	p, m, err := r.db.GetProviderAndModel(ctx, providerID, modelID)
	if err != nil {
		return nil, err
	}
	if !p.Enabled || !m.Enabled {
		return nil, fmt.Errorf("provider or model disabled")
	}
	region := ""
	if p.Region != nil {
		region = *p.Region
	}
	return &core.RoutingTarget{
		ProviderID:         p.ID,
		ProviderName:       p.Name,
		AdapterType:        p.AdapterType,
		ModelID:            m.ID,
		ModelCode:          m.Code,
		ModelName:          m.Name,
		ProviderModelID:    m.ProviderModelID,
		BaseURL:            p.BaseURL,
		Region:             region,
		ServesResponsesAPI: p.ServesResponsesAPI,
	}, nil
}

// LookupTargetFunc returns a TargetLookup suitable for strategy registration.
func (r *Resolver) LookupTargetFunc() core.TargetLookup {
	return r.lookupTarget
}

// Explain runs the full routing pipeline (Resolve) and additionally
// enumerates every terminal target reachable from the matched primary rule
// with its selection probability, so the simulate endpoint can show the
// full branch distribution of stochastic strategies (loadbalance, ab_split,
// conditional). Explain is intended for simulate / operator tooling only;
// live traffic should keep using Resolve (single stochastic pick, no
// redundant tree walk).
func (r *Resolver) Explain(ctx context.Context, rctx *core.RoutingContext) (*core.RoutingPlan, error) {
	plan, err := r.Resolve(ctx, rctx)
	if err != nil {
		return nil, err
	}
	if plan.RuleID == "" {
		return plan, nil
	}

	// Re-fetch to find the matched primary rule's config. Re-using the cached
	// rules list (same call that Resolve made a moment ago) is cheap.
	rules, err := r.db.GetEnabledRoutingRules(ctx)
	if err != nil {
		return plan, nil //nolint:nilerr // Explain is best-effort; return Resolve output.
	}
	for i := range rules {
		if rules[i].ID != plan.RuleID {
			continue
		}
		var node core.StrategyNode
		if jerr := json.Unmarshal(rules[i].Config, &node); jerr != nil {
			return plan, nil //nolint:nilerr // Explain is best-effort; ignore stale rule config.
		}
		plan.Branches = matcher.EnumerateTerminalTargets(ctx, node, rctx, r.lookupTarget)
		return plan, nil
	}
	return plan, nil
}

// ResolveTargets is a higher-level entry point that takes a fully-built
// RoutingContext, runs the routing pipeline via Resolve, then flattens
// the primary + recovery targets into one health-ranked slice for the
// handler's executor.
//
// Callers are expected to populate rctx.Request with the canonical
// normalized payload so smart routing (and future content-aware
// strategies) can inspect the user prompt directly.
//
// When the embeddings capability pre-filter rejected every candidate, this
// returns *core.NoCompatibleProviderError so the proxy handler can emit a
// rich 400 error with available_capabilities.
func (r *Resolver) ResolveTargets(ctx context.Context, rctx *core.RoutingContext) (*core.RouteResult, error) {
	plan, err := r.Resolve(ctx, rctx)
	if err != nil {
		return nil, err
	}

	// Flatten: primary targets + recovery targets.
	allTargets := make([]core.RoutingTarget, 0, len(plan.Targets)+len(plan.RecoveryTargets))
	allTargets = append(allTargets, plan.Targets...)
	allTargets = append(allTargets, plan.RecoveryTargets...)

	// When the embeddings capability pre-filter has rejected every target,
	// return a structured error so the handler can surface available_capabilities.
	if rctx.EndpointType == typology.EndpointKindEmbeddings && r.capCache != nil && rctx.EmbeddingRequest != nil && len(allTargets) == 0 {
		return nil, r.buildNoCompatibleProviderError(ctx, plan, rctx)
	}

	// Health-aware reorder.
	if r.healthRanker != nil {
		allTargets = r.healthRanker.Reorder(allTargets)
	}

	reqModelID, reqProviderID, reqProviderName := requestedIdentity(rctx.RequestedModel)

	return &core.RouteResult{
		Targets:               allTargets,
		Trace:                 plan.Trace,
		PipelineTrace:         plan.PipelineTrace,
		RuleID:                plan.RuleID,
		RuleName:              plan.RuleName,
		Substituted:           plan.Substituted,
		OriginalModelID:       plan.OriginalModelID,
		RequestedModelID:      reqModelID,
		RequestedProviderID:   reqProviderID,
		RequestedProviderName: reqProviderName,
		RuleRetryPolicyJSON:   plan.RuleRetryPolicyJSON,
	}, nil
}

// buildNoCompatibleProviderError constructs a *core.NoCompatibleProviderError
// by re-running the capability filter on all targets from the plan (including
// recovery targets that were filtered in Resolve) to collect rejected candidate
// capability projections for the 400 error body.
func (r *Resolver) buildNoCompatibleProviderError(ctx context.Context, plan *core.RoutingPlan, rctx *core.RoutingContext) *core.NoCompatibleProviderError {
	snap := r.capCache.Load()
	embReq := &capability.EmbeddingRequest{
		Dimensions:     rctx.EmbeddingRequest.Dimensions,
		BatchSize:      rctx.EmbeddingRequest.BatchSize,
		EncodingFormat: rctx.EmbeddingRequest.EncodingFormat,
		InputType:      rctx.EmbeddingRequest.InputType,
		TaskType:       rctx.EmbeddingRequest.TaskType,
	}

	// Re-fetch routing rules to find all candidates before filtering.
	// We need the pre-filter candidates; since plan.Targets is already
	// filtered to zero, we use the plan's trace to identify which models
	// were evaluated. Simpler: run the rejection pass against any targets
	// that appear in plan (they were already rejected; we just need their
	// capability projections). Use plan.Targets + plan.RecoveryTargets as
	// the source (these are the narrowed+filtered-to-zero set).
	//
	// If both are empty (e.g. no rule matched), return the error with empty
	// Available so the handler still writes a well-formed 400.
	// Concatenate into a fresh slice so we don't accidentally extend plan.Targets'
	// backing array (appendAssign).
	allCandidates := make([]core.RoutingTarget, 0, len(plan.Targets)+len(plan.RecoveryTargets))
	allCandidates = append(allCandidates, plan.Targets...)
	allCandidates = append(allCandidates, plan.RecoveryTargets...)

	// Also rebuild from Trace entries when the plan has no targets (rule
	// matched but all targets were narrowed away before our filter ran).
	// The Trace captures each strategy evaluation but not the model IDs
	// in a parseable form — skip the re-fetch and return empty Available.

	available := make([]core.CandidateCapability, 0, len(allCandidates))
	for _, t := range allCandidates {
		capMC := snap.Get(t.ModelID)
		_, _, proj := capability.Compatible(embReq, capMC)
		available = append(available, core.CandidateCapability{
			Provider:                 t.ProviderName,
			Model:                    t.ModelCode,
			SupportedDimensions:      proj.SupportedDimensions,
			MaxBatchSize:             proj.MaxBatchSize,
			SupportedEncodingFormats: proj.SupportedEncodingFormats,
			RequiredExtensions:       proj.RequiredExtensions,
		})
	}
	return &core.NoCompatibleProviderError{Available: available}
}
