package routing

import (
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"hash/fnv"
	"log/slog"
	"slices"
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
	db           routingStore
	registry     *strategies.StrategyRegistry
	logger       *slog.Logger
	vkAccess     matcher.VKAccessFilter
	healthRanker *core.HealthRanker
	// capCache is the atomically swappable capability snapshot used by the
	// embeddings pre-filter. Nil disables the pre-filter so callers
	// that do not wire a capability cache (tests, degraded paths) are not
	// affected.
	capCache *capability.Cache
	// enforceNamedModelModality decides whether the modality FLOOR is applied
	// to a target the caller NAMED (callerNamedTheModel — a direct id or a
	// single-target rule). Default false: a named model's modality verdict is
	// the upstream's, because the floor is a claim about our catalogue and the
	// caller owns the model they named. True enforces the floor for named
	// targets too. It never changes the `auto`/smart path, which filters
	// unconditionally because the gateway chose the model. See
	// config.Routing.EnforceNamedModelModality.
	enforceNamedModelModality bool
	// modelPool resolves the catalogue a pool-needing strategy reads. Wired
	// separately from the strategies that consume it, because the pool is a
	// fact about the REQUEST — assembled where the rest of the request context
	// is — and not a private resource of whichever strategy happens to run.
	// Nil leaves the pool unprepared, and every reader treats that as
	// "unavailable" rather than "no models".
	modelPool core.SmartStore
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
//
// enforceNamedModelModality is the fleet-wide flag governing whether a NAMED
// model's modality floor is enforced by the gateway (true) or deferred to the
// upstream (false, the default). It does not affect the `auto`/smart path.
func NewResolver(catalog routingStore, registry *strategies.StrategyRegistry, healthRanker *core.HealthRanker, logger *slog.Logger, capCache *capability.Cache, enforceNamedModelModality bool) *Resolver {
	return &Resolver{
		db:                        catalog,
		registry:                  registry,
		logger:                    logger,
		vkAccess:                  matcher.VKAccessFilter{},
		healthRanker:              healthRanker,
		capCache:                  capCache,
		enforceNamedModelModality: enforceNamedModelModality,
	}
}

// WithModelPool wires the catalogue source the routing context is prepared
// from. Optional: without it the pool stays nil and a strategy that needs one
// falls back to its own fetch.
func (r *Resolver) WithModelPool(s core.SmartStore) *Resolver {
	r.modelPool = s
	return r
}

// Resolve runs the full routing pipeline and returns a RoutingPlan.
func (r *Resolver) Resolve(ctx context.Context, rctx *core.RoutingContext) (*core.RoutingPlan, error) {
	r.hydrateRequestedModel(ctx, rctx)
	r.prepareModelPool(ctx, rctx)
	plan := &core.RoutingPlan{
		OriginalModelID: rctx.RequestedModel.ID,
	}

	// Fetch all enabled routing rules (cached).
	allRules, err := r.db.GetEnabledRoutingRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("router: fetch rules: %w", err)
	}

	// --- Stage 1: Route decision ---
	stage1Start := time.Now()

	// Every matching rule goes in one ordered list. A rule whose strategyType
	// was `fallback` used to be a separate species appended to every plan as
	// recovery whatever it matched — so it backed up rules it had nothing to do
	// with, and its own match conditions were half-ignored. A rule an admin
	// wrote at lower priority IS the alternative for when the rule above cannot
	// serve. The `fallback` STRATEGY is untouched: a chain of leaves inside one
	// rule is a different thing from a rule that backs up other rules.
	var matched []*store.RoutingRule
	for i := range allRules {
		rule := &allRules[i]
		if rule.PipelineStage != 1 {
			continue
		}
		if !r.ruleMatches(*rule, rctx.RequestedModel.ID, rctx) {
			continue
		}
		matched = append(matched, rule)
	}

	// A model the caller NAMED has to be one this key may use, whether or not a
	// rule would redirect it elsewhere. Checking only the served target answers
	// "what may run" and leaves "what the caller was told" unanswered: a key
	// allowed one model, plus a rule redirecting everything to it, answered a
	// client pinned to another model with a 200.
	//
	// Only for a NAMED model — `auto` never appears on an allow list, and a
	// code fanning out to several providers has no single reference to match,
	// so a blanket rule would refuse every routed request from a restricted key.
	if callerNamedTheModel(rctx.RequestedModel) && rctx.VirtualKey != nil &&
		len(rctx.VirtualKey.AllowedModels) > 0 {
		named := rctx.RequestedModel.CandidateIDs[0]
		if !r.namedModelIsAllowed(ctx, named, rctx.VirtualKey.AllowedModels) {
			return nil, &core.ModelNotAllowedError{RequestedModel: rctx.RequestedModel.ID}
		}
	}

	r.selectRules(ctx, matched, rctx, plan)

	// Measured HERE, before any filter runs, and the placement IS the field.
	// Every filter below empties the plan for OUR reasons — a modality the
	// targets cannot carry, a parameter none of them accept — which is not the
	// rule refusing anything. Counting after them made a catch-all rule take
	// down every non-chat endpoint: it matched, resolved a chat target, the
	// modality filter dropped it, and an image request passthrough serves
	// correctly got a 503.
	plan.RuleMatchedAndResolvedNothing =
		len(matched) > 0 && len(plan.Targets) == 0 && len(plan.RecoveryTargets) == 0

	// When WE choose the model, the choice holds for the whole chain. A caller
	// naming a model owns its limits and gets the upstream's own refusal; a
	// caller sending `auto` asked us to pick, so every target we hand the
	// executor is ours, fallbacks included.
	//
	// Fallbacks were the gap: FallbackChain entries and fallback RULES are built
	// outside the strategy that applied the capability filter. Measured — a
	// trace reading "dropped 44/51 candidates" and "selected gpt-audio-mini"
	// ended with routed_model_name = moonshot-v1-128k on an audio request.
	//
	// plan.Targets gets the FLOOR only. The ceiling is a claim about the
	// catalogue and an explicit pick is honoured rather than second-guessed;
	// the floor is a claim about the model, so a target that requires what this
	// request does not carry cannot serve it whoever chose it, and dispatching
	// anyway buys a round trip and an upstream error we could have named.
	//
	// WHO owns that verdict depends on who chose the model. When the gateway
	// chose it (`auto`, an empty model, a code fanning out to several rows) the
	// choice is ours and we must hand the executor a target that can serve, so
	// the floor is enforced unconditionally. When the CALLER named the model,
	// the floor is still a real property of that model — but it reads from our
	// catalogue, which has been wrong (mislabelled video, reasoning), and the
	// caller owns the model they named. So by default a named target is
	// forwarded and the upstream gives its own verdict; enforceNamedModelModality
	// flips that to enforce the floor for named targets too. The recovery list
	// followed this rule already (it skipped named callers); the primary
	// targets now follow the same predicate, so both agree.
	//
	// Nil cache or snapshot means no opinion, and no opinion keeps every target.
	if r.capCache != nil {
		if snap := r.capCache.Load(); snap != nil {
			carried := capability.CarriedModalities(rctx.Request)
			enforceFloor := !callerNamedTheModel(rctx.RequestedModel) || r.enforceNamedModelModality
			if len(plan.Targets) > 0 && enforceFloor {
				plan.Targets = filterByFloor(snap, plan.Targets, carried, &plan.Trace)
			}
			if len(plan.RecoveryTargets) > 0 && enforceFloor {
				plan.RecoveryTargets = filterRecoveryByCapability(
					snap, plan.RecoveryTargets, carried, &plan.Trace)
			}
		}
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
	maxOut := 0
	if m.MaxOutputTokens != nil {
		maxOut = *m.MaxOutputTokens
	}
	maxCtx := 0
	if m.MaxContextTokens != nil {
		maxCtx = *m.MaxContextTokens
	}
	return &core.RoutingTarget{
		ProviderID:         p.ID,
		ProviderName:       p.Name,
		AdapterType:        p.AdapterType,
		ModelID:            m.ID,
		ModelCode:          m.Code,
		ModelName:          m.Name,
		ModelType:          m.Type,
		ProviderModelID:    m.ProviderModelID,
		BaseURL:            p.BaseURL,
		Region:             region,
		ServesResponsesAPI: p.ServesResponsesAPI,
		Reasons:            slices.Contains(m.Features, core.FeatureReasoning),
		MaxOutputTokens:    maxOut,
		MaxContextTokens:   maxCtx,
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

// would take down the strategies that never wanted a pool.
func (r *Resolver) prepareModelPool(ctx context.Context, rctx *core.RoutingContext) {
	if r.modelPool == nil || rctx == nil {
		return
	}
	pool, err := r.modelPool.ListEnabledCandidates(ctx, rctx.EndpointType)
	if err != nil {
		r.logger.Debug("routing: model pool unavailable; strategies that need one will say so",
			"error", err)
		return
	}
	rctx.ModelPool = pool
}
