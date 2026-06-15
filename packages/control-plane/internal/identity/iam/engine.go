package iam

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// EvaluationResult holds the outcome of a policy evaluation.
type EvaluationResult struct {
	Decision          string             `json:"decision"` // "Allow" or "Deny"
	MatchedStatements []MatchedStatement `json:"matchedStatements"`
	Reason            string             `json:"reason"`
	// CacheHit reports whether the principal's policy set was served from
	// the in-memory cache (true) or freshly loaded from the database
	// (false). Surfaced to feed the iam.eval_total{cache} ops-metrics
	// label; not part of the IAM JSON response surface (omitted from
	// trip reports because clients have no use for it).
	CacheHit bool `json:"-"`
}

// MatchedStatement records which policy statement contributed to the decision.
type MatchedStatement struct {
	PolicyID   string `json:"policyId"`
	PolicyName string `json:"policyName"`
	Sid        string `json:"sid,omitempty"`
	Effect     string `json:"effect"`
	Source     string `json:"source"` // "direct" or "group"
	GroupName  string `json:"groupName,omitempty"`
}

// LoadedPolicy is a policy document with source metadata.
type LoadedPolicy struct {
	ID        string
	Name      string
	Document  PolicyDocument
	Source    string // "direct" or "group"
	GroupName string
}

// PolicyLoader loads policies for a principal from the database.
type PolicyLoader interface {
	LoadPolicies(ctx context.Context, principalType, principalID string) ([]LoadedPolicy, error)
}

// Engine evaluates IAM policies against actions and resources.
type Engine struct {
	loader PolicyLoader
	logger *slog.Logger
	cache  *PolicyCache
}

// EngineOption configures optional Engine behaviour.
type EngineOption func(*Engine)

// WithRedis enables L2 (Redis) caching for the IAM policy cache.
func WithRedis(rdb redis.UniversalClient) EngineOption {
	return func(e *Engine) {
		e.cache = NewPolicyCache(rdb)
	}
}

// NewEngine creates an IAM evaluation engine. Without options the cache is
// L1-only (in-process). Pass WithRedis to enable L2 (Redis) caching.
func NewEngine(loader PolicyLoader, logger *slog.Logger, opts ...EngineOption) *Engine {
	e := &Engine{
		loader: loader,
		logger: logger,
		cache:  NewPolicyCache(nil),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// InvalidateCache clears cached policies for a specific principal or all.
func (e *Engine) InvalidateCache(principalType, principalID string) {
	if principalType != "" && principalID != "" {
		e.cache.Invalidate(principalType + ":" + principalID)
	} else {
		e.cache.InvalidateAll()
	}
}

// CacheSize returns the number of cached principal policy sets.
func (e *Engine) CacheSize() int {
	return e.cache.Size()
}

// loadPolicies returns the principal's policies plus a hit flag. Hit is true
// iff the in-memory cache served the lookup; on a miss the loader was called
// and the result was (possibly) re-cached.
func (e *Engine) loadPolicies(ctx context.Context, principalType, principalID string) ([]LoadedPolicy, bool, error) {
	key := principalType + ":" + principalID

	if policies, ok := e.cache.Get(key); ok {
		return policies, true, nil
	}

	policies, err := e.loader.LoadPolicies(ctx, principalType, principalID)
	if err != nil {
		return nil, false, err
	}

	// Only cache non-empty results (empty may be transient, e.g. during seed).
	if len(policies) > 0 {
		e.cache.Put(key, policies)
	}

	return policies, false, nil
}

// Evaluate checks whether a principal is allowed to perform an action on a resource.
// Backward-compatible wrapper around EvaluateMulti for single-candidate sites.
func (e *Engine) Evaluate(ctx context.Context, principalType, principalID, action, resource string, condCtx ConditionContext) (*EvaluationResult, error) {
	return e.EvaluateMulti(ctx, principalType, principalID, action, []string{resource}, condCtx)
}

// EvaluateMulti is the candidate-list form of Evaluate: a Statement matches
// when its Action covers the request action AND any of its Resource
// patterns matches ANY of the candidate resources. Used by request-time
// IAM checks where the same request can be satisfied by an unscoped grant
// OR by a group-scoped grant — the device's group memberships expand
// into per-group candidate NRNs, the unscoped form stays in the list,
// and a Statement allowing either form authorises the call.
//
// Explicit-deny semantics: a Statement with Effect=Deny that matches
// ANY candidate denies the whole request — denies are intentionally
// broader than allows so an admin can author a Deny scoped to one
// group and have it override unscoped grants. This is the AWS-IAM
// pattern.
func (e *Engine) EvaluateMulti(ctx context.Context, principalType, principalID, action string, resources []string, condCtx ConditionContext) (*EvaluationResult, error) {
	if len(resources) == 0 {
		// Defensive — an empty candidate list should not silently allow.
		resources = []string{"*"}
	}
	// Backfill nexus:CurrentTime centrally so Date-conditioned
	// statements (DateLessThan/DateGreaterThan) evaluate against the real wall
	// clock. Call sites that left it empty/unset previously made every
	// time-windowed Deny silently fail-open (an empty actual fails time.Parse →
	// the Date condition is false → the Deny never matches). A caller that sets
	// an explicit value keeps it.
	if condCtx == nil {
		condCtx = ConditionContext{}
	}
	if condCtx["nexus:CurrentTime"] == "" {
		condCtx["nexus:CurrentTime"] = time.Now().UTC().Format(time.RFC3339)
	}
	policies, cacheHit, err := e.loadPolicies(ctx, principalType, principalID)
	if err != nil {
		return nil, err
	}

	var matched []MatchedStatement
	hasAllow := false
	hasDeny := false

	for _, policy := range policies {
		for _, stmt := range policy.Document.Statement {
			if !matchAction(stmt.Action, action) {
				continue
			}
			if !matchResourceAny(stmt.Resource, resources) {
				continue
			}
			if !EvaluateConditions(stmt.Condition, condCtx) {
				continue
			}

			ms := MatchedStatement{
				PolicyID:   policy.ID,
				PolicyName: policy.Name,
				Sid:        stmt.Sid,
				Effect:     stmt.Effect,
				Source:     policy.Source,
				GroupName:  policy.GroupName,
			}
			matched = append(matched, ms)

			switch stmt.Effect {
			case "Deny":
				hasDeny = true
			case "Allow":
				hasAllow = true
			}
		}
	}

	// Explicit deny > explicit allow > default deny
	if hasDeny {
		return &EvaluationResult{
			Decision:          "Deny",
			MatchedStatements: matched,
			Reason:            "Explicit Deny in matched policy overrides Allow",
			CacheHit:          cacheHit,
		}, nil
	}
	if hasAllow {
		return &EvaluationResult{
			Decision:          "Allow",
			MatchedStatements: matched,
			Reason:            "Allowed by matched policy statement",
			CacheHit:          cacheHit,
		}, nil
	}

	reason := "No matching Allow statement found (default deny)"
	if len(policies) == 0 {
		reason = "No policies attached to principal"
	}
	return &EvaluationResult{
		Decision:          "Deny",
		MatchedStatements: nil,
		Reason:            reason,
		CacheHit:          cacheHit,
	}, nil
}

func matchAction(patterns []string, action string) bool {
	for _, p := range patterns {
		if globMatch(p, action) {
			return true
		}
	}
	return false
}

func matchResource(patterns []string, resource string) bool {
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		if MatchNRN(p, resource) {
			return true
		}
	}
	return false
}

// matchResourceAny returns true when at least one (pattern, resource)
// pair matches across the Cartesian product. Used by EvaluateMulti for
// the candidate-list form of request resources (unscoped + group-scoped).
func matchResourceAny(patterns []string, resources []string) bool {
	for _, r := range resources {
		if matchResource(patterns, r) {
			return true
		}
	}
	return false
}
