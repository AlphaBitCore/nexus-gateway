package quota

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
)

// Engine is the new hierarchical quota enforcement engine.
// It checks quota limits across multiple levels (VK, user/project,
// organization hierarchy) using cached policies and Redis-backed usage counters.
//
// FAIL-OPEN POSTURE (deliberate availability tradeoff). When the
// usage-cache (Redis) read fails during Check, the engine skips that level
// rather than rejecting the request: a cache outage degrades the gateway to
// *unmetered* rather than *down*. The cost of that choice is that during a
// Redis outage cost caps are not enforced and spend is unbounded. The cost is
// METERED, not silent: every fail-open skip increments
// quota_check_failopen_total{reason="redis_error"} so ops can alert on the
// window. The authoritative counter reconciles on the next Backfill once Redis
// recovers, so the discrepancy is transient.
//
// SOFT-RESERVATION / POST-SUCCESS SETTLEMENT MODEL. Check is a
// READ-ONLY pre-check: it reads currentCents and compares currentCents +
// estimate against the limit, but it does NOT reserve the estimate. The usage
// counter advances only in Reconcile, after the upstream call succeeds. The
// consequence is that N requests arriving concurrently all read the same
// currentCents, all pass the pre-check, and all dispatch — so the cap can be
// overshot. The overshoot is BOUNDED by (in-flight concurrency × per-request
// cost), not unbounded: once Reconcile settles the in-flight batch, subsequent
// requests see the advanced counter and trip the cap. This is not a hole — it
// is an availability/accuracy tradeoff. A reserve-then-settle design (decrement
// a reservation on dispatch, refund on failure) would tighten the bound to
// exactly the limit but adds a distributed reservation-ledger with refund and
// crash-recovery semantics; that redesign is out of scope here. The bounded
// post-success model is documented and intentional.
type Engine struct {
	policyCache *PolicyCache
	usageCache  *UsageCache
	logger      *slog.Logger
	metrics     *Metrics // nil-safe: nil = no observability (sibling-package tests)

	// carry holds the sub-cent reconcile remainder so sub-cent-per-call
	// models (mini/flash/haiku-class, embeddings) still advance the cent
	// counter instead of truncating to zero every Reconcile. Keyed by
	// "targetType:targetID"; the value tracks the active period plus the
	// 0..999 milli-cent remainder not yet committed as a whole cent. The
	// map is bounded by the active-subject working set (one entry per VK /
	// user / project / org that has reconciled) — not multiplied by period,
	// since a period rollover resets the entry in place. A reset drops at
	// most <1 cent of residual, which is negligible against a cost cap.
	carryMu sync.Mutex
	carry   map[string]carryEntry
}

// carryEntry is the per-subject sub-cent reconcile remainder.
type carryEntry struct {
	periodKey string // the period the remainder belongs to
	milli     int64  // 0..999 milli-cents not yet rolled into a whole cent
}

// NewEngine creates an Engine with the given caches. metrics may be nil
// (observability disabled — used by sibling-package tests that do not stand up
// a Prometheus registry); production wiring passes a registered *Metrics.
func NewEngine(policyCache *PolicyCache, usageCache *UsageCache, logger *slog.Logger, metrics *Metrics) *Engine {
	return &Engine{
		policyCache: policyCache,
		usageCache:  usageCache,
		logger:      logger,
		metrics:     metrics,
		carry:       make(map[string]carryEntry),
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
		// A blank cost or period on the override inherits from the matching
		// policy rather than shadowing it — mirrors Check so the /v1/usage
		// quota block and the request-time headers stay consistent (doc §7,
		// §10). Without the cost fallback a blank-cost override reports
		// hasLimit=false while Check still enforces the policy cap.
		if limitCents <= 0 || pType == "" {
			if policy := e.policyCache.FindPolicy("virtual_key", vkMeta.OrganizationID, vkMeta.VKType); policy != nil {
				if limitCents <= 0 {
					limitCents = policy.CostLimitCents
				}
				if pType == "" {
					pType = policy.PeriodType
				}
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
	// Fail-open on a usage-cache (Redis) read error: report currentCents=0
	// rather than fail the usage query. The discrepancy is bounded and the
	// rollup reconciles the authoritative counter; we log at warn so a cache
	// outage is observable instead of being swallowed silently.
	currentCents, err := e.usageCache.GetUsage(ctx, "virtual_key", vkMeta.ID, periodKey)
	if err != nil {
		e.logger.Warn("quota: usage-cache read failed; reporting current usage as 0 (deliberate fail-open)",
			"error", err,
			"target", "virtual_key:"+vkMeta.ID,
			"periodKey", periodKey)
		currentCents = 0
	}
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

	// If the policy cache never loaded (a boot-time DB failure left
	// it empty), every level below resolves no limit and we allow unconditionally
	// — a silent, persistent fail-open. Emit the alertable fail-open counter so
	// the unenforced window is visible to ops (the empty cache itself self-heals
	// via the background reload started in InitQuota). This is distinct from a
	// legitimately empty (no-policies) config, which leaves loaded=true.
	if !e.policyCache.Loaded() {
		e.metrics.observeCheckFailOpen("policy_cache_unloaded")
	}

	// vkMeta is dereferenced below to resolve org/VK-scoped policies. A nil
	// vkMeta has no policy context to enforce against, so allow rather than
	// panic — mirrors VKLimit's nil guard.
	if vkMeta == nil {
		e.metrics.observeDecision(decision.Action)
		return decision
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

			// Inherit any unspecified field from the matching policy. A blank
			// cost, enforcement mode, or period on the override must fall back
			// to the policy — a blank cost especially must NOT shadow the
			// policy's cost cap (doc §7), which would silently disable cost
			// enforcement at this level.
			if limitCents <= 0 || enforcementMode == "" || periodType == "" {
				policy := e.policyCache.FindPolicy(level.TargetType, vkMeta.OrganizationID, vkMeta.VKType)
				if policy != nil {
					if limitCents <= 0 {
						limitCents = policy.CostLimitCents
					}
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
			// Fail open on cache errors (deliberate — see the package-level
			// FAIL-OPEN POSTURE note on Engine). Metered via the counter so the
			// unenforced window is alertable instead of silent.
			e.metrics.observeCheckFailOpen("redis_error")
			e.logger.Error("get usage cache",
				"error", err,
				"target", level.TargetType+":"+level.TargetID)
			continue
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

	e.metrics.observeDecision(decision.Action)
	return decision
}

// Reconcile updates usage counters for all levels after a request completes.
//
// Each level is incremented under its OWN stamped period key (Check stamps a
// per-level PeriodKey; a mixed-period chain — e.g. VK monthly + org daily —
// must advance each counter under its own period or a level silently stops
// enforcing). Levels with no matching policy/override (unstamped period) fall
// back to the decision-wide period so their measurement counter still moves.
//
// Cost is converted to milli-cents and a per-subject remainder is carried
// across calls so sub-cent-per-call costs accumulate into whole cents instead
// of truncating to zero every reconcile.
func (e *Engine) Reconcile(ctx context.Context, decision *Decision, actual ActualUsage) {
	costMilliCents := int64(math.Round(actual.ActualCost() * 100_000))
	if costMilliCents <= 0 {
		return
	}

	// Resolve per-level whole-cent commits under each level's own period,
	// carrying the sub-cent remainder per subject. Levels that commit the
	// same whole-cent amount under the same period collapse into a single
	// IncrMulti pipeline call (the common case: a single-period chain with
	// aligned remainders); divergent periods or remainders split into
	// separate buckets.
	type bucketKey struct {
		periodKey string
		cents     int64
	}
	buckets := make(map[bucketKey][]UsageLevel)

	e.carryMu.Lock()
	for _, l := range decision.Levels {
		periodKey := l.PeriodKey
		if periodKey == "" {
			periodKey = decision.PeriodKey
		}

		subjectKey := l.TargetType + ":" + l.TargetID
		entry := e.carry[subjectKey]
		if entry.periodKey != periodKey {
			// Period rolled over (or first sight) — start a fresh remainder.
			entry = carryEntry{periodKey: periodKey}
		}
		total := entry.milli + costMilliCents
		cents := total / 1000
		entry.milli = total % 1000
		e.carry[subjectKey] = entry

		if cents > 0 {
			k := bucketKey{periodKey: periodKey, cents: cents}
			buckets[k] = append(buckets[k], UsageLevel{TargetType: l.TargetType, TargetID: l.TargetID})
		}
	}
	e.carryMu.Unlock()

	for k, levels := range buckets {
		err := e.usageCache.IncrMulti(ctx, levels, k.periodKey, k.cents)
		if err != nil {
			// One bounded retry before giving up: a transient Redis blip
			// (failover, brief network hiccup) often clears within a tick, and
			// a lost reconcile increment is permanent until the next boot
			// Backfill — so a single cheap retry meaningfully cuts counter drift
			// without unbounded retry machinery.
			time.Sleep(100 * time.Millisecond)
			err = e.usageCache.IncrMulti(ctx, levels, k.periodKey, k.cents)
		}
		if err != nil {
			// Increment permanently lost until the next Backfill. Meter it so
			// ops can alert on counter drift instead of discovering it only at
			// reboot.
			e.metrics.observeReconcileFailed()
			e.logger.Error("reconcile usage", "error", err, "periodKey", k.periodKey)
		}
	}
}
