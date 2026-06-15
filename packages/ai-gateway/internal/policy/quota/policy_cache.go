package quota

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// canonicalQuotaScope folds the VK scope token to the single value the
// enforcement engine queries. The CP UI / admin API persists VK-scoped quota
// policies and overrides as "vk", while chain.go / enforcement.go look them up
// as "virtual_key" (the value chain.go emits and metric_rollup_1h uses). Without
// this fold a VK-scoped quota an admin sets in the UI is stored but never
// enforced. All other scope tokens (user / project / organization)
// pass through unchanged.
func canonicalQuotaScope(s string) string {
	if s == "vk" {
		return "virtual_key"
	}
	return s
}

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
	Scope           string // "user" | "virtual_key" | "project" | "organization"
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
	EnforcementMode string     // empty = inherit from policy
	PeriodType      string     // empty = inherit from policy
	ExpiresAt       *time.Time // nil = never expires
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
	// loaded flips true after the FIRST successful Load. It distinguishes
	// "loaded, legitimately zero policies" (enforce nothing, correct) from
	// "never loaded due to a boot-time DB failure" (the silent
	// fail-open) so the engine can emit an alertable signal for the latter.
	loaded atomic.Bool
}

// Loaded reports whether the cache has completed at least one successful Load.
// A false value means the engine is serving against an empty cache because the
// boot load failed — an unenforced (fail-open) window, not a deliberate
// no-policies config.
func (c *PolicyCache) Loaded() bool { return c.loaded.Load() }

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
			// Round, don't truncate: int64($0.29*100) is 28 because 0.29*100 is
			// 28.9999… in float64, silently shaving a cent off every limit.
			// math.Round gives the intended 29.
			p.CostLimitCents = int64(math.Round(*costLimitUsd * 100))
		}
		// Canonicalize the VK scope token. The CP UI/admin API writes
		// VK-scoped rows as "vk"; the enforcement engine (chain.go / enforcement.go)
		// queries "virtual_key". Without this fold a VK-scoped quota set in the UI
		// is silently never enforced.
		p.Scope = canonicalQuotaScope(p.Scope)
		newPolicies[p.Scope] = append(newPolicies[p.Scope], p)
	}

	// Load overrides. The SQL pre-filters rows expired at query time; we also
	// store ExpiresAt in the cache so GetOverride can re-check at enforcement
	// time without waiting for the next Load trigger.
	overrideRows, err := c.pool.Query(ctx, `
		SELECT id, "targetType", "targetId",
		       "costLimitUsd"::double precision, "enforcementMode", "periodType",
		       "expiresAt"
		FROM "QuotaOverride"
		WHERE "expiresAt" IS NULL OR "expiresAt" > NOW()
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
			&costLimitUsd, &enfMode, &periodType, &o.ExpiresAt); err != nil {
			return fmt.Errorf("policy_cache: scan override: %w", err)
		}
		if costLimitUsd != nil {
			// Round, not truncate — same rationale as the policy limit above.
			o.CostLimitCents = int64(math.Round(*costLimitUsd * 100))
		}
		if enfMode != nil {
			o.EnforcementMode = *enfMode
		}
		if periodType != nil {
			o.PeriodType = *periodType
		}
		// Same canonicalization as policies — UI writes "vk",
		// enforcement queries "virtual_key".
		o.TargetType = canonicalQuotaScope(o.TargetType)
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
	c.loaded.Store(true) // First successful load clears the fail-open flag.

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

// ActivePeriodTypes returns the distinct, normalized period types referenced
// by any loaded policy or override (e.g. ["daily","monthly"]). An empty or
// unrecognized PeriodType normalizes to "monthly" (the CurrentPeriodKey
// default), so the result always contains at least "monthly" when any limit
// exists. Backfill uses this to re-seed every active period's counter on boot,
// not just the monthly one.
func (c *PolicyCache) ActivePeriodTypes() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	seen := make(map[string]struct{})
	add := func(pt string) {
		switch pt {
		case "daily", "weekly", "monthly":
			seen[pt] = struct{}{}
		default:
			// Empty or unknown -> monthly, mirroring CurrentPeriodKey's default.
			seen["monthly"] = struct{}{}
		}
	}
	for _, ps := range c.policiesByScope {
		for i := range ps {
			add(ps[i].PeriodType)
		}
	}
	for _, o := range c.overridesByKey {
		if o != nil {
			add(o.PeriodType)
		}
	}
	out := make([]string, 0, len(seen))
	for pt := range seen {
		out = append(out, pt)
	}
	return out
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

// GetOverride returns the active override for a specific target, or nil.
// An override whose ExpiresAt is in the past is treated as absent — this
// closes the enforcement gap where a cached override would outlive its
// expiry until the next Load() trigger.
func (c *PolicyCache) GetOverride(targetType, targetID string) *CachedOverride {
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := targetType + ":" + targetID
	o := c.overridesByKey[key]
	if o == nil {
		return nil
	}
	if o.ExpiresAt != nil && time.Now().After(*o.ExpiresAt) {
		return nil
	}
	return o
}
