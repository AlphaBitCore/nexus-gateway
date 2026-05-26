package quota

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
)

// Engine is the new hierarchical quota enforcement engine.
// It checks quota limits across multiple levels (VK, user/project,
// organization hierarchy) using cached policies and Redis-backed usage counters.
type Engine struct {
	policyCache *PolicyCache
	usageCache  *UsageCache
	logger      *slog.Logger
}

// NewEngine creates an Engine with the given caches.
func NewEngine(policyCache *PolicyCache, usageCache *UsageCache, logger *slog.Logger) *Engine {
	return &Engine{
		policyCache: policyCache,
		usageCache:  usageCache,
		logger:      logger,
	}
}

// OrgParents returns the org hierarchy map from the policy cache.
func (e *Engine) OrgParents() map[string]string {
	return e.policyCache.OrgParents()
}

// UsageForTarget returns the current usage in cents for a given target and period.
func (e *Engine) UsageForTarget(ctx context.Context, targetType, targetID, periodKey string) (int64, error) {
	return e.usageCache.GetUsage(ctx, targetType, targetID, periodKey)
}

// VKLimit resolves the active VK-level quota for the given VKMeta and
// returns the limit, current period usage, period key, and whether a
// matching override or policy exists. `hasLimit` is false when no
// override or policy applies to this VK; callers should treat that as
// "no quota visibility on this VK".
//
// Used by the /v1/usage handler to build the quota block without
// running a full Check + estimate pre-flight.
func (e *Engine) VKLimit(ctx context.Context, vkMeta *vkauth.VKMeta) (limitCents int64, currentCents int64, periodKey string, hasLimit bool) {
	if vkMeta == nil {
		return 0, 0, "", false
	}
	override := e.policyCache.GetOverride("virtual_key", vkMeta.ID)
	var pType string
	if override != nil {
		limitCents = override.CostLimitCents
		pType = override.PeriodType
		if pType == "" {
			if policy := e.policyCache.FindPolicy("virtual_key", vkMeta.OrganizationID, vkMeta.VKType); policy != nil {
				pType = policy.PeriodType
			}
		}
	} else {
		policy := e.policyCache.FindPolicy("virtual_key", vkMeta.OrganizationID, vkMeta.VKType)
		if policy == nil {
			return 0, 0, "", false
		}
		limitCents = policy.CostLimitCents
		pType = policy.PeriodType
	}
	if limitCents <= 0 {
		return 0, 0, "", false
	}
	periodKey = CurrentPeriodKey(pType)
	currentCents, _ = e.usageCache.GetUsage(ctx, "virtual_key", vkMeta.ID, periodKey)
	return limitCents, currentCents, periodKey, true
}

// Decision is the result of a quota check.
type Decision struct {
	Allowed   bool
	Action    string // "allow" | "reject" | "downgrade" | "notify-and-proceed" | "track-only"
	Message   string
	QuotaID   string       // which level triggered the action
	Levels    []CheckLevel // all levels checked (for reconciliation)
	PeriodKey string
}

// CurrentPeriodKey returns the period key for the given period type.
func CurrentPeriodKey(periodType string) string {
	now := time.Now().UTC()
	switch periodType {
	case "daily":
		return now.Format("2006-01-02")
	case "weekly":
		y, w := now.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w)
	default: // monthly
		return now.Format("2006-01")
	}
}

// actionPriority returns higher number for more restrictive actions.
func actionPriority(action string) int {
	switch action {
	case "reject":
		return 4
	case "downgrade":
		return 3
	case "notify-and-proceed":
		return 2
	case "track-only":
		return 1
	default:
		return 0
	}
}

// Check performs hierarchical quota enforcement across all levels in the chain.
// It returns a Decision indicating whether the request should proceed and
// what action to take (allow, reject, downgrade, notify).
func (e *Engine) Check(ctx context.Context, chain []CheckLevel, estimate CostEstimate, vkMeta *vkauth.VKMeta) *Decision {
	decision := &Decision{
		Allowed: true,
		Action:  "allow",
		Levels:  chain,
	}

	estimatedCents := int64(estimate.EstimatedCost() * 100)

	for i, level := range chain {
		// 1. Resolve limit: override takes precedence, then policy.
		override := e.policyCache.GetOverride(level.TargetType, level.TargetID)

		var limitCents int64
		var enforcementMode string
		var periodType string
		var quotaID string

		if override != nil {
			limitCents = override.CostLimitCents
			enforcementMode = override.EnforcementMode
			periodType = override.PeriodType
			quotaID = "override:" + override.ID

			// Inherit from policy if override doesn't specify enforcement or period.
			if enforcementMode == "" || periodType == "" {
				policy := e.policyCache.FindPolicy(level.TargetType, vkMeta.OrganizationID, vkMeta.VKType)
				if policy != nil {
					if enforcementMode == "" {
						enforcementMode = policy.EnforcementMode
					}
					if periodType == "" {
						periodType = policy.PeriodType
					}
				}
			}
		} else {
			policy := e.policyCache.FindPolicy(level.TargetType, vkMeta.OrganizationID, vkMeta.VKType)
			if policy == nil {
				continue // No limit at this level.
			}
			limitCents = policy.CostLimitCents
			enforcementMode = policy.EnforcementMode
			periodType = policy.PeriodType
			quotaID = "policy:" + policy.ID
		}

		if limitCents <= 0 || enforcementMode == "track-only" {
			continue // No limit or track-only = don't enforce.
		}

		periodKey := CurrentPeriodKey(periodType)
		if decision.PeriodKey == "" {
			decision.PeriodKey = periodKey
		}

		// 2. Get current usage from cache.
		currentCents, err := e.usageCache.GetUsage(ctx, level.TargetType, level.TargetID, periodKey)
		if err != nil {
			e.logger.Error("get usage cache",
				"error", err,
				"target", level.TargetType+":"+level.TargetID)
			continue // Fail open on cache errors.
		}

		// Stamp the resolved limit + current usage onto the level so callers
		// (e.g. proxy header emit) can read it from Decision.Levels without
		// a second usage-cache round-trip.
		chain[i].HasLimit = true
		chain[i].CurrentCents = currentCents
		chain[i].LimitCents = limitCents
		chain[i].PeriodKey = periodKey

		// 3. Check if over limit.
		if currentCents+estimatedCents > limitCents {
			if actionPriority(enforcementMode) > actionPriority(decision.Action) {
				decision.Action = enforcementMode
				decision.QuotaID = quotaID
				decision.Message = fmt.Sprintf("%s quota exceeded: %s (%.2f / %.2f USD)",
					level.TargetType, level.TargetID,
					float64(currentCents)/100, float64(limitCents)/100)
				if enforcementMode == "reject" || enforcementMode == "downgrade" {
					decision.Allowed = false
				}
			}
		}
	}

	// Set period key to monthly default if not set.
	if decision.PeriodKey == "" {
		decision.PeriodKey = CurrentPeriodKey("monthly")
	}

	return decision
}

// Reconcile updates usage counters for all levels after a request completes.
func (e *Engine) Reconcile(ctx context.Context, decision *Decision, actual ActualUsage) {
	costCents := int64(actual.ActualCost() * 100)
	if costCents <= 0 {
		return
	}

	levels := make([]UsageLevel, len(decision.Levels))
	for i, l := range decision.Levels {
		levels[i] = UsageLevel{TargetType: l.TargetType, TargetID: l.TargetID}
	}

	if err := e.usageCache.IncrMulti(ctx, levels, decision.PeriodKey, costCents); err != nil {
		e.logger.Error("reconcile usage", "error", err)
	}
}
