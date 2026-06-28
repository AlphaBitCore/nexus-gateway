package proxy

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
)

// proxy_routing_quota.go holds the quota cost-estimate and downgrade-budget
// helpers used by checkQuota (in proxy_routing.go). Behavior unchanged.

// estimateTokens approximates the input token count of a request body for the
// quota PRE-CHECK cost estimate only — which is explicitly an approximation,
// reconciled to the actual usage post-call (see quota-architecture.md §6), so
// precision is not required. It uses bytes/3 rather than utf8.RuneCount(body)/3:
// counting runes over the whole ~50 KB body was ~7% of request-path CPU, and the
// byte length needs no scan. bytes/3 equals the prior rune/3 for ASCII and
// slightly OVER-estimates multi-byte UTF-8 (CJK), which is the safe direction for
// a quota pre-check (fails closed marginally earlier rather than under-reserving).
func estimateTokens(body []byte) int64 {
	est := int64(len(body)) / 3
	if est < 1 {
		est = 1
	}
	return est
}

// quotaHasCostLimit reports whether any level in the decision carries an
// enforced cost limit (Engine.Check stamps HasLimit on every level that
// resolved a positive cost cap). Used by the unpriced-model guard
// to fail closed only when a cost quota actually applies.
func quotaHasCostLimit(decision *quota.Decision) bool {
	if decision == nil {
		return false
	}
	for _, lvl := range decision.Levels {
		if lvl.HasLimit {
			return true
		}
	}
	return false
}

// quotaDowngradeBudget returns, in USD, the remaining headroom under the
// tightest enforced cap in the decision — the maximum spend a downgraded
// model may incur while still satisfying EVERY level's cost cap. Levels
// without a limit are ignored; a level already at/over its cap contributes
// 0 (forcing selection of the cheapest available model). Returns 0 when no
// level carries a limit.
func quotaDowngradeBudget(decision *quota.Decision) float64 {
	if decision == nil {
		return 0
	}
	budgetCents := int64(-1)
	for _, lvl := range decision.Levels {
		if !lvl.HasLimit {
			continue
		}
		remaining := lvl.LimitCents - lvl.CurrentCents
		if remaining < 0 {
			remaining = 0
		}
		if budgetCents < 0 || remaining < budgetCents {
			budgetCents = remaining
		}
	}
	if budgetCents < 0 {
		budgetCents = 0
	}
	return float64(budgetCents) / 100
}
