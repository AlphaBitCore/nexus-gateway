// Package builtins provides the default HookRegistry pre-loaded with all
// built-in compliance hook factories. Import this package to obtain Registry.
//
// Every implementation registered here must be platform-agnostic Go and
// depend only on shared/. Service-specific variants (e.g. those that need
// a shared HTTP-client pool) override via Registry.Clone().Replace() at
// consumer setup time so all three data-plane services share the same logic.
package builtins

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/access"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/ratelimit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/validators"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/webhook"
)

// Registry is the default global registry with all built-in hooks registered.
// It is frozen at init time and safe for concurrent read access.
var Registry = func() *core.HookRegistry {
	r := core.NewHookRegistry()
	r.Register("keyword-filter", validators.NewKeywordFilter)
	r.Register("pii-detector", validators.NewPiiDetector)
	r.Register("content-safety", validators.NewContentSafety)
	r.Register("rate-limiter", ratelimit.NewRateLimiter)
	r.Register("request-size-validator", validators.NewRequestSizeValidator)
	r.Register("ip-access-filter", access.NewIPAccessFilter)
	r.Register("data-residency", access.NewDataResidency)
	r.Register("rulepack-engine", validators.NewRulePackEngine)
	r.Register("noop", core.NewNoop)
	r.Register("webhook-forward", webhook.NewWebhookForward)
	r.Register("quality-checker", validators.NewQualityChecker)
	r.Freeze()
	return r
}()
