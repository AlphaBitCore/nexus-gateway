package proxy

// proxy_retrypolicy.go — routing-rule retry-policy resolution helpers split out
// of proxy.go to keep that file under its size ratchet. These turn a routing
// rule's stored retryPolicy JSON into the effective cfgpolicy.RetryPolicy the
// executor honours (YAML default field-merged with the per-rule override).

import (
	"log/slog"

	"github.com/goccy/go-json"

	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

// parseRulePolicy unmarshals a routing rule's stored retryPolicy JSON
// into a *cfgpolicy.RetryPolicy. Returns nil for empty/null/invalid
// JSON; an unparseable value is logged but does not fail the request —
// the rule simply inherits the YAML default.
func (h *Handler) parseRulePolicy(raw json.RawMessage) *cfgpolicy.RetryPolicy {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var p cfgpolicy.RetryPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		if h != nil && h.deps != nil && h.deps.Logger != nil {
			h.deps.Logger.Warn("routing rule retryPolicy JSON unparseable; falling back to YAML default",
				slog.String("error", err.Error()),
				slog.String("raw", string(raw)),
			)
		}
		return nil
	}
	return &p
}

// effectiveRetryPolicy returns the policy the executor should honor for
// this request: the YAML default field-merged with the matched rule's
// per-rule override (if any). When the deps are missing a default
// (e.g. tests that did not wire RoutingDefaultPolicy), this falls back
// to cfgpolicy.DefaultRetryPolicy() so the executor never runs with a
// zero-valued policy (which would clamp MaxAttemptsPerTarget to 1 with
// nil RetryOn — "retry everything once" — instead of the documented
// platform defaults).
func (h *Handler) effectiveRetryPolicy(raw json.RawMessage, logger *slog.Logger) cfgpolicy.RetryPolicy {
	base := cfgpolicy.DefaultRetryPolicy()
	if h != nil && h.deps != nil {
		// Treat an all-zero RoutingDefaultPolicy as "not wired" — main.go
		// always populates it from cfg.Routing.DefaultRetryPolicy, which
		// the config loader merges against DefaultRetryPolicy() so a
		// real deployment always carries non-zero fields.
		if dp := h.deps.RoutingDefaultPolicy; !dp.IsZero() {
			base = dp
		}
	}
	rule := h.parseRulePolicy(raw)
	policy := base.MergedWith(rule)
	policy.MaxAttemptsPerTarget = cfgpolicy.ClampMaxAttempts(policy.MaxAttemptsPerTarget)
	if logger != nil && rule != nil {
		logger.Debug("retry policy merged",
			slog.Int("maxAttemptsPerTarget", policy.MaxAttemptsPerTarget),
			slog.Any("retryOn", policy.RetryOn),
			slog.Duration("backoffInitial", policy.BackoffInitial),
			slog.Duration("backoffMax", policy.BackoffMax),
			slog.Float64("backoffJitter", policy.BackoffJitter),
		)
	}
	return policy
}
