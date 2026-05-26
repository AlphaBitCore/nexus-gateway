// packages/ai-gateway/internal/policy/aiguard/inproc.go
package aiguard

import "context"

// InProcClient bundles the AIGuard dependencies and exposes a single
// Classify(ctx, req) method suitable for in-process ai-gateway hook
// callers (P-D prompt-injection, P-E quality-checker). It is a thin
// composition over Classify() + ConfigCache + backend selection.
//
// Construction happens once in main.go; the same client is shared by
// all ai-gateway hooks.
//
// Reconcile responsibility: Classify returns AI-Guard's raw suggested
// Decision with no policy context. Any caller that integrates the
// returned Decision into a hook pipeline result MUST reconcile it
// against the hook's own OnMatchConfig.InflightAction ceiling — see
// the webhook-forward implementation in
// packages/shared/policy/hooks/webhook/webhook.go for the canonical
// pattern (core.StrictestDecision + ReasonAIGuardSuggestedVsPolicy
// stamp on divergence). When P-D / P-E land, factor the reconcile
// into a shared helper so the rule is enforced once instead of
// per-consumer.
type InProcClient struct {
	configCache *ConfigCache
	backendFor  func(cfg *RuntimeConfig) (Backend, error)
	cache       *Cache
	sink        TrafficSink
}

// NewInProcClient constructs the client. backendFor is called per classify
// invocation to return the currently-configured backend — isolating backend
// selection so the wiring in main.go stays in one place.
func NewInProcClient(
	cc *ConfigCache, backendFor func(cfg *RuntimeConfig) (Backend, error),
	cache *Cache, sink TrafficSink,
) *InProcClient {
	return &InProcClient{configCache: cc, backendFor: backendFor, cache: cache, sink: sink}
}

// Classify runs the classification pipeline and returns the Response.
// Errors on config load / backend resolution / backend failure.
func (c *InProcClient) Classify(ctx context.Context, req Request) (*Response, error) {
	full, err := c.configCache.Get(ctx)
	if err != nil {
		return nil, err
	}
	rc := &RuntimeConfig{
		BackendMode:        full.BackendMode,
		BackendFingerprint: full.BackendFingerprint,
		PromptTemplate:     full.PromptTemplate,
		TimeoutMs:          full.TimeoutMs,
		CacheTTLSeconds:    full.CacheTTLSeconds,
		InputStrategy:      full.InputStrategy,
		ModelContextLimit:  full.ModelContextLimit,
	}
	be, err := c.backendFor(rc)
	if err != nil {
		return nil, err
	}
	return Classify(ctx, req, rc, be, c.cache, c.sink)
}
