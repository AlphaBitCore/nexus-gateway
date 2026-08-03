package strategies

import (
	"context"
	"fmt"
	"time"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/inputstaging"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// defaultOutputReserveTokens is the completion-space reserve used by the
// context-window filter when the request carries no maxTokens: the
// selected model must fit the estimated input plus at least this much
// output. Kept modest on purpose — over-reserving would filter out
// models that could actually serve the request.
const defaultOutputReserveTokens = 1024

// estimateRequestTokens approximates the upstream context cost of the
// full canonical request: text in every role (assistant/tool history
// usually dominates, not the user turn), tool-use input JSON, tool-result
// output, and tool definitions. Image blocks are counted (images) but not
// token-estimated — the character heuristic has no meaningful mapping for
// binary refs, and the filter must stay fail-open on what it cannot
// measure; the count feeds the router's request-metadata line.
//
// It uses the CONSERVATIVE estimate, never the average-case one: this count
// gates filterByContextWindow, where under-counting is the one unrecoverable
// error — it admits a model whose context cannot hold the request, which then
// hard-400s upstream (observed: an auto-routed body counted 216543 tokens
// against a 131072-limit model that the 0.25/char average had sized under
// 128k). Over-counting only selects a larger-context model, which succeeds.
func estimateRequestTokens(p *normcore.NormalizedPayload) (tokens, images int) {
	for _, m := range p.Messages {
		for _, b := range m.Content {
			tokens += inputstaging.EstimateTokensConservative(b.Text)
			if b.ImageRef != nil {
				images++
			}
			if b.ToolUse != nil {
				tokens += inputstaging.EstimateTokensConservative(b.ToolUse.Name)
				if len(b.ToolUse.Input) > 0 {
					if j, err := json.Marshal(b.ToolUse.Input); err == nil {
						tokens += inputstaging.EstimateTokensConservative(string(j))
					}
				}
			}
			if b.ToolResult != nil {
				tokens += inputstaging.EstimateTokensConservative(b.ToolResult.Output)
			}
		}
	}
	for _, td := range p.Tools {
		tokens += inputstaging.EstimateTokensConservative(td.Name)
		tokens += inputstaging.EstimateTokensConservative(td.Description)
		if len(td.ParametersJSONSchema) > 0 {
			if j, err := json.Marshal(td.ParametersJSONSchema); err == nil {
				tokens += inputstaging.EstimateTokensConservative(string(j))
			}
		}
	}
	return tokens, images
}

// filterByContextWindow keeps candidates whose declared context window
// holds the required token estimate. Candidates without a declared
// MaxContextTokens pass (fail-open: the filter only acts on data the
// catalog has). When no candidate fits, the largest-context candidates
// are kept instead of emptying the pool — the estimate is coarse, and
// the largest model gives the request its best chance, whereas falling
// back to a (typically small) default would guarantee the overflow.
func filterByContextWindow(candidates []core.SmartModelRow, required int) (kept []core.SmartModelRow, dropped int, noneFit bool) {
	for _, c := range candidates {
		if c.MaxContextTokens == nil || *c.MaxContextTokens >= required {
			kept = append(kept, c)
		}
	}
	if len(kept) > 0 || len(candidates) == 0 {
		return kept, len(candidates) - len(kept), false
	}
	// Every candidate declared a too-small window (a nil MaxContextTokens
	// would have been kept above). Keep the largest tier, ties included.
	maxCtx := 0
	for _, c := range candidates {
		if *c.MaxContextTokens > maxCtx {
			maxCtx = *c.MaxContextTokens
		}
	}
	for _, c := range candidates {
		if *c.MaxContextTokens == maxCtx {
			kept = append(kept, c)
		}
	}
	return kept, len(candidates) - len(kept), true
}

// Feature tags a candidate must declare when the request provably needs
// the capability. Values match the Model.features vocabulary.
const (
	featureVision          = "vision"
	featureFunctionCalling = "function_calling"
)

// filterByCapability keeps candidates that can serve the request's
// provable capability needs: vision when the request carries image
// blocks, function_calling when it declares tools. Only candidates that
// DECLARE a feature list are judged — an empty list passes (fail-open:
// absent catalog metadata must not disqualify). A dimension that would
// empty the pool is skipped entirely (fail-open); skipped dimension
// names are returned so the caller can trace them.
func filterByCapability(candidates []core.SmartModelRow, needsVision, needsTools bool) (kept []core.SmartModelRow, dropped int, skippedDims []string) {
	kept = candidates
	for _, dim := range []struct {
		needed  bool
		feature string
	}{
		{needsVision, featureVision},
		{needsTools, featureFunctionCalling},
	} {
		if !dim.needed {
			continue
		}
		var pass []core.SmartModelRow
		for _, c := range kept {
			if len(c.Features) == 0 || containsFeature(c.Features, dim.feature) {
				pass = append(pass, c)
			}
		}
		if len(pass) == 0 {
			skippedDims = append(skippedDims, dim.feature)
			continue
		}
		dropped += len(kept) - len(pass)
		kept = pass
	}
	return kept, dropped, skippedDims
}

func containsFeature(features []string, want string) bool {
	for _, f := range features {
		if f == want {
			return true
		}
	}
	return false
}

// armContextUpgrade returns the picked target, plus — when a candidate
// in the same filtered (VK- and capability-respecting) pool declares a
// strictly larger context window — the largest such candidate marked
// ContextUpgradeOnly. The size estimate feeding the context filter is
// coarse, so the pick can still overflow upstream; the executor fails
// over to the armed target exactly on a context-overflow verdict and
// never for transient failures.
func (s *SmartStrategy) armContextUpgrade(ctx context.Context, candidates []core.SmartModelRow, selected *core.SmartModelRow, target *core.RoutingTarget, trace *[]core.TraceEntry, start time.Time) []core.RoutingTarget {
	targets := []core.RoutingTarget{*target}
	if selected.MaxContextTokens == nil {
		return targets
	}
	var up *core.SmartModelRow
	for i := range candidates {
		c := &candidates[i]
		if c.ModelID == selected.ModelID || c.MaxContextTokens == nil {
			continue
		}
		if *c.MaxContextTokens > *selected.MaxContextTokens && (up == nil || *c.MaxContextTokens > *up.MaxContextTokens) {
			up = c
		}
	}
	if up == nil {
		return targets
	}
	upTarget, err := s.deps.Lookup(ctx, up.ProviderID, up.ModelID)
	if err != nil {
		return targets
	}
	upTarget.ContextUpgradeOnly = true
	targets = append(targets, *upTarget)
	*trace = append(*trace, core.TraceEntry{
		StrategyType: "smart",
		Decision:     fmt.Sprintf("context-upgrade armed: %s [%s/%s] (window %d > %d)", core.FormatTargetFriendly(upTarget), up.ProviderID, up.ModelID, *up.MaxContextTokens, *selected.MaxContextTokens),
		DurationMs:   int(time.Since(start).Milliseconds()),
	})
	return targets
}
