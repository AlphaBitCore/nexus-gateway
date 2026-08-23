// resolve_targets.go — the second half of routing: a plan, once built, is
// filtered by what this endpoint can carry, ordered by health, and handed to
// the caller as a RouteResult. resolver.go builds the plan; this decides which
// of it survives and in what order.
package routing

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/capability"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

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

	// Each list is filtered on its own, then concatenated: filtering the two
	// together would let a modality drop in the chain be scored against the
	// strategy's own picks.
	ranked, rankedDropped := filterByModality(plan.Targets, rctx.EndpointType)
	fallback, fallbackDropped := filterByModality(plan.RecoveryTargets, rctx.EndpointType)
	r.reportModalityDrops(plan, rctx, rankedDropped+fallbackDropped, len(ranked)+len(fallback))

	// The structured embeddings 400 belongs to requests WE routed. It lists what
	// each candidate would have accepted, which answers "you asked us to pick
	// and none of ours fit". A caller who named a model asked a different
	// question, and handing them a catalogue of models they did not choose both
	// reads as a gateway that ignored them and publishes the deployment's model
	// list to someone who wanted one model.
	if rctx.EndpointType == typology.EndpointKindEmbeddings && r.capCache != nil &&
		rctx.EmbeddingRequest != nil && !callerNamedTheModel(rctx.RequestedModel) &&
		len(ranked) == 0 && len(fallback) == 0 {
		return nil, r.buildNoCompatibleProviderError(ctx, plan, rctx)
	}

	// Concatenated with the duplicates removed, keeping the FIRST occurrence.
	//
	// The two lists are assembled independently — the strategy's answer, and
	// the rule's own chain — so a chain naming a model the strategy also picked
	// put that model in the plan twice. It is not cosmetic: the walk gives each
	// entry its own state and dispatches to it again after a transient failure,
	// and the call budget is derived from the plan's LENGTH, so a duplicate
	// silently buys the request another pair of attempts. First occurrence
	// wins because that is the position the strategy and the health reorder
	// chose for it.
	allTargets := make([]core.RoutingTarget, 0, len(ranked)+len(fallback))
	seenTarget := make(map[[2]string]bool, len(ranked)+len(fallback))
	for _, t := range append(append([]core.RoutingTarget{}, ranked...), fallback...) {
		key := [2]string{t.ProviderID, t.ModelID}
		if seenTarget[key] {
			continue
		}
		seenTarget[key] = true
		allTargets = append(allTargets, t)
	}

	// Health ordering spans the whole plan, not each list: a primary whose
	// provider is answering nothing has to be demoted BELOW the chain, and that
	// demotion crosses the boundary between the strategy's answer and the rule's
	// backups. Role survives the reorder on each target (Source).
	if r.healthRanker != nil {
		allTargets = r.healthRanker.Reorder(allTargets)
	}

	reqModelID, reqProviderID, reqProviderName := requestedIdentity(rctx.RequestedModel)

	return &core.RouteResult{
		Dispatch:                      allTargets,
		Trace:                         plan.Trace,
		PipelineTrace:                 plan.PipelineTrace,
		RuleID:                        plan.RuleID,
		RuleName:                      plan.RuleName,
		Substituted:                   plan.Substituted,
		OriginalModelID:               plan.OriginalModelID,
		RequestedModelID:              reqModelID,
		RequestedProviderID:           reqProviderID,
		RequestedProviderName:         reqProviderName,
		RuleRetryPolicyJSON:           plan.RuleRetryPolicyJSON,
		RuleMatchedAndResolvedNothing: plan.RuleMatchedAndResolvedNothing,
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
			MinDimension:             proj.MinDimension,
			MaxDimension:             proj.MaxDimension,
			MaxBatchSize:             proj.MaxBatchSize,
			SupportedEncodingFormats: proj.SupportedEncodingFormats,
			RequiredExtensions:       proj.RequiredExtensions,
		})
	}
	return &core.NoCompatibleProviderError{Available: available}
}

// prepareModelPool reads, once per request and FOR THE REQUEST'S ENDPOINT KIND,
// the catalogue any strategy that needs a pool will use. Reading the chat set
// unconditionally cost a non-chat `auto` request two reads from two snapshots.
//
// Deliberately NOT narrowed by the virtual key: the router model routes traffic
// and is not itself a candidate, so its declared window must be readable even
// for a key that cannot use it — narrowing first reads it as zero and silently
// shrinks the catalogue prompt for exactly the restricted keys. Candidacy
// narrowing is the strategy's.
//
// A failure leaves the pool nil, which readers distinguish from empty. Refusing
