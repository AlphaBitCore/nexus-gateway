package store

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// RoutingRule represents a routing rule from the database.
type RoutingRule struct {
	ID              string
	Name            string
	StrategyType    string
	Config          json.RawMessage // StrategyNode tree
	MatchConditions json.RawMessage // MatchConditions JSON (nullable) — sole rule-matching truth source.
	Priority        int
	PipelineStage   int
	FallbackChain   json.RawMessage // [{providerId, modelId}] (nullable)
	// RetryPolicy is the per-rule override for executor retry behavior,
	// stored as JSONB in the RoutingRule.retryPolicy column. Null/empty
	// means "use the YAML default (cfg.Routing.DefaultRetryPolicy) as-is".
	// When set, configtypes.RetryPolicy.MergedWith field-merges its set
	// fields on top of the YAML default at evaluation time.
	RetryPolicy json.RawMessage
	Enabled     bool
}

// rulesCache caches all enabled routing rules with TTL.
//
// `loaded` (not `rules != nil`) is the freshness predicate. A deployment
// with zero enabled routing rules is a legitimate, common state — it is
// the shipped default and what the smoke harness configures for its
// routing-OFF phases — and loadRoutingRules returns a nil slice for it.
// Keying the hit on `rules != nil` therefore treated "cached: no rules"
// as "never loaded" and re-queried Postgres on every single lookup — and
// the request path performs one or two of them (route resolution always,
// plus the smart-routing canonical probe when lazy canonicalization is
// on). The flag makes the negative result cacheable and distinguishable
// from "not loaded yet".
type rulesCache struct {
	mu        sync.RWMutex
	rules     []RoutingRule
	loaded    bool
	expiresAt time.Time
	sfg       singleflight.Group
}

const rulesCacheTTL = 30 * time.Minute

// GetEnabledRoutingRules returns all enabled routing rules ordered by
// pipelineStage ASC, priority DESC. Results are cached. Uses singleflight
// to coalesce concurrent cache-miss refreshes.
func (db *DB) GetEnabledRoutingRules(ctx context.Context) ([]RoutingRule, error) {
	db.initRulesCache()

	db.rc.mu.RLock()
	if db.rc.loaded && time.Now().Before(db.rc.expiresAt) {
		rules := db.rc.rules
		db.rc.mu.RUnlock()
		return rules, nil
	}
	db.rc.mu.RUnlock()

	v, err, _ := db.rc.sfg.Do("load", func() (any, error) {
		return db.loadRoutingRules(ctx)
	})
	if err != nil {
		return nil, err
	}

	rules := v.([]RoutingRule)
	db.rc.mu.Lock()
	db.rc.rules = rules
	db.rc.loaded = true
	db.rc.expiresAt = time.Now().Add(rulesCacheTTL)
	db.rc.mu.Unlock()

	return rules, nil
}

// loadRoutingRules fetches enabled routing rules from the database.
func (db *DB) loadRoutingRules(ctx context.Context) ([]RoutingRule, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, name, "strategyType", config, "matchConditions",
		       priority, "pipelineStage", "fallbackChain", "retryPolicy",
		       enabled
		FROM "RoutingRule"
		WHERE enabled = true
		ORDER BY "pipelineStage" ASC, priority DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("store: get routing rules: %w", err)
	}
	defer rows.Close()

	var rules []RoutingRule
	for rows.Next() {
		var r RoutingRule
		if err := rows.Scan(&r.ID, &r.Name, &r.StrategyType, &r.Config,
			&r.MatchConditions, &r.Priority, &r.PipelineStage,
			&r.FallbackChain, &r.RetryPolicy, &r.Enabled); err != nil {
			return nil, fmt.Errorf("store: scan routing rule: %w", err)
		}
		rules = append(rules, r)
	}
	return rules, nil
}

// InvalidateRuleCache forces a re-fetch on the next call.
func (db *DB) InvalidateRuleCache() {
	db.initRulesCache()
	db.rc.mu.Lock()
	defer db.rc.mu.Unlock()
	db.rc.expiresAt = time.Time{}
}

func (db *DB) initRulesCache() {
	db.rcOnce.Do(func() {
		db.rc = &rulesCache{}
	})
}
