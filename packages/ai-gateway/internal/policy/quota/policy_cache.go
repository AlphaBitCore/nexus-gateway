package quota

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgxPool is the minimum pgx pool surface this package needs at runtime.
// The concrete *pgxpool.Pool satisfies it in production; pgxmock's
// PgxPoolIface satisfies it in tests. Mirrors the seam used by
// packages/ai-gateway/internal/store and packages/control-plane/internal/store.
type PgxPool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// CachedPolicy holds a policy loaded from QuotaPolicy table.
type CachedPolicy struct {
	ID              string
	Scope           string // "user" | "virtual_key" | "project" | "organization"|"user" | "virtual_key" | "project" | "organization"|"user" | "virtual_key" | "project" | "organization"|"user" | "virtual_key" | "project" | "organization"
	OrganizationID  string // empty = match all
	VKType          string // empty = match all
	PeriodType      string
	CostLimitCents  int64 // stored as cents to avoid float
	EnforcementMode string
	Priority        int
}

// CachedOverride holds an override loaded from QuotaOverride table.
type CachedOverride struct {
	ID              string
	TargetType      string
	TargetID        string
	CostLimitCents  int64
	EnforcementMode string // empty = inherit from policy
	PeriodType      string // empty = inherit from policy
}

// PolicyCache holds all enabled quota policies and overrides in memory,
// refreshed on demand via Load (triggered by config invalidation).
type PolicyCache struct {
	mu              sync.RWMutex
	policiesByScope map[string][]CachedPolicy  // keyed by scope, sorted by priority DESC
	overridesByKey  map[string]*CachedOverride // keyed by "targetType:targetId"
	orgParents      map[string]string          // orgID -> parentOrgID
	pool            PgxPool
	logger          *slog.Logger
}

// PolicySnapshot returns all cached policies (across every scope) flattened
// into a single slice for runtime introspection (e31-s7). Order is by
// scope then policy priority — same as the on-disk view.
func (c *PolicyCache) PolicySnapshot() []CachedPolicy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	total := 0
	for _, ps := range c.policiesByScope {
		total += len(ps)
	}
	out := make([]CachedPolicy, 0, total)
	for _, ps := range c.policiesByScope {
		out = append(out, ps...)
	}
	return out
}

// OverrideSnapshot returns all cached overrides for runtime introspection.
func (c *PolicyCache) OverrideSnapshot() []CachedOverride {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]CachedOverride, 0, len(c.overridesByKey))
	for _, o := range c.overridesByKey {
		if o == nil {
			continue
		}
		out = append(out, *o)
	}
	return out
}

// NewPolicyCache creates a PolicyCache backed by the given connection pool.
// Accepts the concrete *pgxpool.Pool used in production; pass nil for tests
// that exercise only the in-memory paths.
func NewPolicyCache(pool *pgxpool.Pool, logger *slog.Logger) *PolicyCache {
	c := &PolicyCache{
		policiesByScope: make(map[string][]CachedPolicy),
		overridesByKey:  make(map[string]*CachedOverride),
		orgParents:      make(map[string]string),
		logger:          logger,
	}
	// pool may be a typed-nil *pgxpool.Pool; storing it in the interface
	// field would make `c.pool == nil` false. Avoid that by storing only
	// when the concrete pointer is non-nil.
	if pool != nil {
		c.pool = pool
	}
	return c
}

// NewPolicyCacheWithPgxPool is the test-only constructor that accepts
// any pgx-compatible pool (e.g. pgxmock's PgxPoolIface). Production code
// must use NewPolicyCache with a real *pgxpool.Pool.
func NewPolicyCacheWithPgxPool(pool PgxPool, logger *slog.Logger) *PolicyCache {
	return &PolicyCache{
		policiesByScope: make(map[string][]CachedPolicy),
		overridesByKey:  make(map[string]*CachedOverride),
		orgParents:      make(map[string]string),
		pool:            pool,
		logger:          logger,
	}
}

// Load reads all enabled policies, all overrides, and the org tree from DB
// into memory.
func (c *PolicyCache) Load(ctx context.Context) error {
	if c.pool == nil {
		return nil
	}

	// Load policies.
	policyRows, err := c.pool.Query(ctx, `
		SELECT id, scope, "organizationId", "vkType", "periodType",
		       "costLimitUsd"::double precision, "enforcementMode", priority
		FROM "QuotaPolicy"
		WHERE enabled = true
		ORDER BY priority DESC
	`)
	if err != nil {
		return fmt.Errorf("policy_cache: query policies: %w", err)
	}
	defer policyRows.Close()

	newPolicies := make(map[string][]CachedPolicy)
	for policyRows.Next() {
		var p CachedPolicy
		var orgID, vkType *string
		var costLimitUsd *float64
		if err := policyRows.Scan(&p.ID, &p.Scope, &orgID, &vkType,
			&p.PeriodType, &costLimitUsd, &p.EnforcementMode, &p.Priority); err != nil {
			return fmt.Errorf("policy_cache: scan policy: %w", err)
		}
		if orgID != nil {
			p.OrganizationID = *orgID
		}
		if vkType != nil {
			p.VKType = *vkType
		}
		if costLimitUsd != nil {
			p.CostLimitCents = int64(*costLimitUsd * 100)
		}
		newPolicies[p.Scope] = append(newPolicies[p.Scope], p)
	}

	// Load overrides.
	overrideRows, err := c.pool.Query(ctx, `
		SELECT id, "targetType", "targetId",
		       "costLimitUsd"::double precision, "enforcementMode", "periodType"
		FROM "QuotaOverride"
	`)
	if err != nil {
		return fmt.Errorf("policy_cache: query overrides: %w", err)
	}
	defer overrideRows.Close()

	newOverrides := make(map[string]*CachedOverride)
	for overrideRows.Next() {
		var o CachedOverride
		var costLimitUsd *float64
		var enfMode, periodType *string
		if err := overrideRows.Scan(&o.ID, &o.TargetType, &o.TargetID,
			&costLimitUsd, &enfMode, &periodType); err != nil {
			return fmt.Errorf("policy_cache: scan override: %w", err)
		}
		if costLimitUsd != nil {
			o.CostLimitCents = int64(*costLimitUsd * 100)
		}
		if enfMode != nil {
			o.EnforcementMode = *enfMode
		}
		if periodType != nil {
			o.PeriodType = *periodType
		}
		key := o.TargetType + ":" + o.TargetID
		newOverrides[key] = &o
	}

	// Load org tree for hierarchy traversal.
	orgRows, err := c.pool.Query(ctx, `SELECT id, COALESCE("parentId", '') FROM "Organization"`)
	if err != nil {
		return fmt.Errorf("policy_cache: load org tree: %w", err)
	}
	defer orgRows.Close()

	parents := make(map[string]string)
	for orgRows.Next() {
		var id, parentID string
		if err := orgRows.Scan(&id, &parentID); err != nil {
			return fmt.Errorf("policy_cache: scan org: %w", err)
		}
		parents[id] = parentID
	}

	// Swap atomically.
	c.mu.Lock()
	c.policiesByScope = newPolicies
	c.overridesByKey = newOverrides
	c.orgParents = parents
	c.mu.Unlock()

	c.logger.Info("policy cache loaded",
		"policies", countPolicies(newPolicies),
		"overrides", len(newOverrides),
		"orgs", len(parents))
	return nil
}

func countPolicies(m map[string][]CachedPolicy) int {
	n := 0
	for _, v := range m {
		n += len(v)
	}
	return n
}

// OrgParents returns a copy of the org hierarchy map (orgID -> parentOrgID).
func (c *PolicyCache) OrgParents() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m := make(map[string]string, len(c.orgParents))
	for k, v := range c.orgParents {
		m[k] = v
	}
	return m
}

// SetPoliciesForTest installs a per-scope policy table into the cache,
// replacing whatever Load would have read. Intended exclusively for
// tests in sibling packages (handler, etc.) that drive Engine.Check
// branches without standing up a real database — production code MUST
// go through Load. The policies slice for each scope is used in iteration
// order; callers should pre-sort by priority DESC to mirror the SQL
// ordering Load applies.
func (c *PolicyCache) SetPoliciesForTest(scopeToPolicies map[string][]CachedPolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.policiesByScope = make(map[string][]CachedPolicy, len(scopeToPolicies))
	for scope, ps := range scopeToPolicies {
		copyPs := make([]CachedPolicy, len(ps))
		copy(copyPs, ps)
		c.policiesByScope[scope] = copyPs
	}
}

// SetOverridesForTest installs the override table into the cache,
// replacing whatever Load would have read. Same testing contract as
// SetPoliciesForTest. The map key is the "{targetType}:{targetID}" form
// already used internally.
func (c *PolicyCache) SetOverridesForTest(keyed map[string]*CachedOverride) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.overridesByKey = make(map[string]*CachedOverride, len(keyed))
	for k, v := range keyed {
		if v == nil {
			continue
		}
		cp := *v
		c.overridesByKey[k] = &cp
	}
}

// SetOrgParentsForTest installs the org hierarchy map into the cache,
// replacing whatever Load would have read. Same testing contract as
// SetPoliciesForTest.
func (c *PolicyCache) SetOrgParentsForTest(parents map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.orgParents = make(map[string]string, len(parents))
	for k, v := range parents {
		c.orgParents[k] = v
	}
}

// FindPolicy finds the highest-priority matching policy for a scope + context.
func (c *PolicyCache) FindPolicy(scope, organizationID, vkType string) *CachedPolicy {
	c.mu.RLock()
	defer c.mu.RUnlock()

	policies, ok := c.policiesByScope[scope]
	if !ok {
		return nil
	}
	// Policies are already sorted by priority DESC from the query.
	for i := range policies {
		p := &policies[i]
		if p.OrganizationID != "" && p.OrganizationID != organizationID {
			continue
		}
		if p.VKType != "" && p.VKType != vkType {
			continue
		}
		return p
	}
	return nil
}

// GetOverride returns the override for a specific target, or nil.
func (c *PolicyCache) GetOverride(targetType, targetID string) *CachedOverride {
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := targetType + ":" + targetID
	return c.overridesByKey[key]
}
