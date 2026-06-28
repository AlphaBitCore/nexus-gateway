// resolver_canonical.go — the lazy-canonical routing gate: whether a
// content-reading rule (smart) could apply to a given model, so the proxy
// materializes the request canonical only when a rule will actually read it.
// Split from resolver.go (Resolve / target lookup / capability filter).
package routing

import (
	"bytes"
	"context"
	"github.com/goccy/go-json"
	"hash/fnv"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
)

// contentCacheEntry caches whether a rule Config's StrategyNode tree reads the
// canonical request payload. raw is retained for byte-verification against the
// rare FNV-64 collision.
type contentCacheEntry struct {
	raw   []byte
	reads bool
}

// configReadsContent reports whether a rule's Config (StrategyNode tree) reads
// the canonical request payload, memoizing the recursive walk by a hash of the
// raw Config bytes (mirrors parseMatchConditions). Fail-safe inside
// core.ConfigReadsContent: malformed Config returns true.
func (r *Resolver) configReadsContent(config json.RawMessage) bool {
	if len(config) == 0 {
		return false
	}
	h := fnv.New64a()
	_, _ = h.Write(config)
	key := h.Sum64()
	if v, ok := r.contentCache.Load(key); ok {
		if e := v.(*contentCacheEntry); bytes.Equal(e.raw, config) {
			return e.reads
		}
	}
	reads := core.ConfigReadsContent(config)
	r.contentCache.Store(key, &contentCacheEntry{raw: append([]byte(nil), config...), reads: reads})
	return reads
}

// RequestNeedsCanonical reports whether any enabled content-reading routing rule
// (e.g. smart) could apply to a request for modelID — i.e. its model match
// conditions permit this model. The ai-gateway proxy uses this to decide whether
// routing must materialize the request-path canonical (the lazy-canonical gate):
// when no content-reading rule could fire for this model the canonical is never
// computed for routing and the audit writer defers the normalized payload.
//
// The match is MODEL-ONLY: a content-reading rule scoped to other models (e.g.
// requestedModelLiterals=["auto"]) no longer taxes every concrete-model request.
// Non-model conditions (VK / project / header) are intentionally ignored so the
// gate is a conservative superset that never under-computes.
//
// Fail-safe: returns true on any rule-fetch error so a content-reading strategy
// is never starved of its input (a false negative would silently route smart
// rules to their default model). The per-rule walk is memoized by Config hash,
// so steady-state cost is a map lookup over the (tiny) enabled-rule set.
func (r *Resolver) RequestNeedsCanonical(ctx context.Context, modelID string) bool {
	rules, err := r.db.GetEnabledRoutingRules(ctx)
	if err != nil {
		return true
	}
	for i := range rules {
		if !r.configReadsContent(rules[i].Config) {
			continue
		}
		if r.ruleModelCouldMatch(rules[i].MatchConditions, modelID) {
			return true
		}
	}
	return false
}

// ruleModelCouldMatch reports whether a rule could apply to modelID considering
// ONLY the model dimensions of its match conditions. Returns true (conservative)
// for catch-all or malformed conditions, and for model-set / model-type
// conditions that cannot be evaluated before the catalog model is hydrated.
// Returns false only when requestedModelLiterals is present and excludes modelID
// with no other model dimension present — the one cheap, certain exclusion.
func (r *Resolver) ruleModelCouldMatch(raw json.RawMessage, modelID string) bool {
	if len(raw) == 0 {
		return true // catch-all applies to every model
	}
	conds := r.parseMatchConditions(raw)
	if conds == nil {
		return true // malformed → conservative
	}
	if len(conds.Models) > 0 || len(conds.ModelTypes) > 0 {
		// Need the hydrated candidate IDs / model type, unavailable pre-canonical.
		return true
	}
	if len(conds.RequestedModelLiterals) > 0 {
		for _, lit := range conds.RequestedModelLiterals {
			if lit == modelID {
				return true
			}
		}
		return false // literals present and none matched this model
	}
	return true // no model dimension (only VK/header conditions) → could match
}
