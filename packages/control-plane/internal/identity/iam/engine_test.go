package iam

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type mockLoader struct {
	policies []LoadedPolicy
}

func (m *mockLoader) LoadPolicies(_ context.Context, _, _ string) ([]LoadedPolicy, error) {
	return m.policies, nil
}

// errLoader simulates a DB outage so the loadPolicies error branch in
// EvaluateMulti is reached.
type errLoader struct{ err error }

func (e *errLoader) LoadPolicies(_ context.Context, _, _ string) ([]LoadedPolicy, error) {
	return nil, e.err
}

func TestEngineEvaluate(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	tests := []struct {
		name         string
		policies     []LoadedPolicy
		action       string
		resource     string
		wantDecision string
	}{
		{
			name:         "no policies → default deny",
			policies:     nil,
			action:       "admin:ReadProvider",
			resource:     "nrn:nexus:gateway:*:provider/openai",
			wantDecision: "Deny",
		},
		{
			name: "allow matches",
			policies: []LoadedPolicy{{
				ID: "p1", Name: "test", Source: "direct",
				Document: PolicyDocument{
					Version:   PolicyVersion,
					Statement: []Statement{{Effect: "Allow", Action: []string{"admin:*"}, Resource: []string{"nrn:nexus:*:*:*/*"}}},
				},
			}},
			action:       "admin:ReadProvider",
			resource:     "nrn:nexus:gateway:*:provider/openai",
			wantDecision: "Allow",
		},
		{
			name: "explicit deny overrides allow",
			policies: []LoadedPolicy{
				{ID: "p1", Name: "allow-all", Source: "direct",
					Document: PolicyDocument{Version: PolicyVersion, Statement: []Statement{
						{Effect: "Allow", Action: []string{"*"}, Resource: []string{"nrn:nexus:*:*:*/*"}},
					}}},
				{ID: "p2", Name: "deny-write", Source: "direct",
					Document: PolicyDocument{Version: PolicyVersion, Statement: []Statement{
						{Effect: "Deny", Action: []string{"admin:WriteProvider"}, Resource: []string{"nrn:nexus:*:*:*/*"}},
					}}},
			},
			action:       "admin:WriteProvider",
			resource:     "nrn:nexus:gateway:*:provider/openai",
			wantDecision: "Deny",
		},
		{
			name: "action mismatch → deny",
			policies: []LoadedPolicy{{
				ID: "p1", Name: "read-only", Source: "direct",
				Document: PolicyDocument{Version: PolicyVersion, Statement: []Statement{
					{Effect: "Allow", Action: []string{"admin:Read*"}, Resource: []string{"nrn:nexus:*:*:*/*"}},
				}},
			}},
			action:       "admin:DeleteProvider",
			resource:     "nrn:nexus:gateway:*:provider/openai",
			wantDecision: "Deny",
		},
		{
			name: "super admin policy",
			policies: []LoadedPolicy{{
				ID: "p1", Name: "NexusSuperAdmin", Source: "direct",
				Document: NexusSuperAdmin,
			}},
			action:       "admin:DeleteProvider",
			resource:     "nrn:nexus:gateway:*:provider/openai",
			wantDecision: "Allow",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := NewEngine(&mockLoader{policies: tt.policies}, logger)
			result, err := engine.Evaluate(context.Background(), "api_key", "test-key", tt.action, tt.resource, nil)
			if err != nil {
				t.Fatalf("Evaluate error: %v", err)
			}
			if result.Decision != tt.wantDecision {
				t.Errorf("decision = %q, want %q (reason: %s)", result.Decision, tt.wantDecision, result.Reason)
			}
		})
	}
}

// TestEngineEvaluate_BackfillsCurrentTime locks SEC-M7-01: a Deny guarded by a
// Date condition on nexus:CurrentTime fires even when the caller passes nil/empty
// condCtx, because Evaluate backfills the real wall clock centrally. Before the
// fix the empty CurrentTime made time.Parse fail → the Date condition was false →
// the Deny never matched → the broad Allow fail-opened.
func TestEngineEvaluate_BackfillsCurrentTime(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	policies := []LoadedPolicy{
		{ID: "allow", Name: "broad-enroll", Source: "direct",
			Document: PolicyDocument{Version: PolicyVersion, Statement: []Statement{
				{Effect: "Allow", Action: []string{"device-enrollment.enroll"}, Resource: []string{"nrn:nexus:*:*:*/*"}},
			}}},
		{ID: "deny", Name: "expired-grant", Source: "direct",
			Document: PolicyDocument{Version: PolicyVersion, Statement: []Statement{
				// "this grant expired at 2000": Deny once CurrentTime is past it.
				{Effect: "Deny", Action: []string{"device-enrollment.enroll"}, Resource: []string{"nrn:nexus:*:*:*/*"},
					Condition: ConditionBlock{"DateGreaterThan": {"nexus:CurrentTime": "2000-01-01T00:00:00Z"}}},
			}}},
	}
	engine := NewEngine(&mockLoader{policies: policies}, logger)
	// nil condCtx — exactly the sso-enroll / iamauth fail-open scenario.
	result, err := engine.Evaluate(context.Background(), "user", "alice", "device-enrollment.enroll", "nrn:nexus:hub:*:thing/agent-1", nil)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if result.Decision != "Deny" {
		t.Errorf("decision = %q, want Deny (time-windowed Deny must fire via central CurrentTime backfill; reason: %s)", result.Decision, result.Reason)
	}
}

// TestEngineEvaluateMulti_GroupScopedSemantics covers the candidate-list
// form of Evaluate used by RequireIAMPermissionForDevice. Verifies the
// AWS-IAM-like semantics: a Statement matches when ANY pattern matches
// ANY candidate; explicit Deny on any candidate-pattern pair wins; an
// unscoped wildcard pattern continues to authorise group-scoped
// candidates so legacy policies stay valid.
func TestEngineEvaluateMulti_GroupScopedSemantics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Policies the test cases below pull from by name.
	allowSingapore := LoadedPolicy{
		ID: "p-sg", Name: "RegionalSG", Source: "direct",
		Document: PolicyDocument{
			Version: PolicyVersion,
			Statement: []Statement{{
				Effect:   "Allow",
				Action:   []string{"admin:agent-device.rotate"},
				Resource: []string{"nrn:nexus:agent:*:agent-device/group:sg/*"},
			}},
		},
	}
	allowFleetWide := LoadedPolicy{
		ID: "p-fleet", Name: "FleetAdmin", Source: "direct",
		Document: PolicyDocument{
			Version: PolicyVersion,
			Statement: []Statement{{
				Effect:   "Allow",
				Action:   []string{"admin:agent-device.*"},
				Resource: []string{"nrn:nexus:agent:*:agent-device/*"},
			}},
		},
	}
	denyOnFrankfurt := LoadedPolicy{
		ID: "p-deny-fra", Name: "DenyFRA", Source: "direct",
		Document: PolicyDocument{
			Version: PolicyVersion,
			Statement: []Statement{{
				Effect:   "Deny",
				Action:   []string{"admin:agent-device.rotate"},
				Resource: []string{"nrn:nexus:agent:*:agent-device/group:fra/*"},
			}},
		},
	}

	tests := []struct {
		name         string
		policies     []LoadedPolicy
		action       string
		resources    []string
		wantDecision string
	}{
		{
			name:     "regional policy allows device in matching group",
			policies: []LoadedPolicy{allowSingapore},
			action:   "admin:agent-device.rotate",
			resources: []string{
				"nrn:nexus:agent:*:agent-device/dev-1",
				"nrn:nexus:agent:*:agent-device/group:sg/dev-1",
			},
			wantDecision: "Allow",
		},
		{
			name:     "regional policy denies device not in any matching group",
			policies: []LoadedPolicy{allowSingapore},
			action:   "admin:agent-device.rotate",
			resources: []string{
				"nrn:nexus:agent:*:agent-device/dev-2",
				"nrn:nexus:agent:*:agent-device/group:fra/dev-2",
			},
			wantDecision: "Deny",
		},
		{
			name:     "fleet-wide policy authorises any candidate (incl. group-scoped)",
			policies: []LoadedPolicy{allowFleetWide},
			action:   "admin:agent-device.rotate",
			resources: []string{
				"nrn:nexus:agent:*:agent-device/dev-3",
				"nrn:nexus:agent:*:agent-device/group:fra/dev-3",
			},
			wantDecision: "Allow",
		},
		{
			name:     "explicit Deny on one candidate group overrides fleet-wide Allow",
			policies: []LoadedPolicy{allowFleetWide, denyOnFrankfurt},
			action:   "admin:agent-device.rotate",
			resources: []string{
				"nrn:nexus:agent:*:agent-device/dev-4",
				"nrn:nexus:agent:*:agent-device/group:fra/dev-4",
			},
			wantDecision: "Deny",
		},
		{
			name:     "device in multiple groups, one matching, one not — still Allow",
			policies: []LoadedPolicy{allowSingapore},
			action:   "admin:agent-device.rotate",
			resources: []string{
				"nrn:nexus:agent:*:agent-device/dev-5",
				"nrn:nexus:agent:*:agent-device/group:sg/dev-5",
				"nrn:nexus:agent:*:agent-device/group:fra/dev-5",
			},
			wantDecision: "Allow",
		},
		{
			// Empty candidate list means the middleware failed to
			// derive scope (bug condition). EvaluateMulti's defensive
			// fallback uses "*" as resource, which does NOT match
			// concrete `agent-device/*` patterns — request fails
			// closed → Deny. This is the safe behaviour: a coding
			// mistake doesn't accidentally authorise a request.
			name:         "empty candidate list fails closed (Deny, no crash)",
			policies:     []LoadedPolicy{allowFleetWide},
			action:       "admin:agent-device.rotate",
			resources:    []string{},
			wantDecision: "Deny",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := NewEngine(&mockLoader{policies: tt.policies}, logger)
			result, err := engine.EvaluateMulti(context.Background(), "api_key", "test-key", tt.action, tt.resources, nil)
			if err != nil {
				t.Fatalf("EvaluateMulti error: %v", err)
			}
			if result.Decision != tt.wantDecision {
				t.Errorf("decision = %q, want %q (reason: %s)", result.Decision, tt.wantDecision, result.Reason)
			}
		})
	}
}

// TestNewEngine_WithRedisInstallsL2Cache covers the WithRedis option:
// the engine's cache must be replaced with a PolicyCache backed by a
// non-nil Redis client. Without this, CP-UI's IAM invalidation channel
// would only purge L1 (in-process) leaving stale rows in L2 on every
// other Control Plane instance.
func TestNewEngine_WithRedisInstallsL2Cache(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	e := NewEngine(&mockLoader{}, logger, WithRedis(rdb))
	if e.cache == nil {
		t.Fatal("WithRedis should install a PolicyCache")
	}
	if e.cache.rdb != rdb {
		t.Error("WithRedis did not pass through the redis client")
	}
}

// TestEngine_InvalidateCacheSpecificAndAll covers the two branches in
// Engine.InvalidateCache: (principalType, principalID) both set →
// targeted Invalidate; either empty → InvalidateAll.
func TestEngine_InvalidateCacheSpecificAndAll(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	e := NewEngine(&mockLoader{policies: []LoadedPolicy{
		{ID: "p1", Name: "test", Source: "direct"},
	}}, logger)

	// Warm the cache by evaluating twice (same principal).
	_, _ = e.Evaluate(context.Background(), "user", "alice", "x:y", "nrn:*", nil)
	_, _ = e.Evaluate(context.Background(), "user", "bob", "x:y", "nrn:*", nil)
	if got := e.CacheSize(); got != 2 {
		t.Fatalf("cache size after warmup: got %d, want 2", got)
	}

	// Targeted invalidate: only alice drops.
	e.InvalidateCache("user", "alice")
	if got := e.CacheSize(); got != 1 {
		t.Fatalf("after targeted invalidate: got %d, want 1", got)
	}

	// Wildcard invalidate (empty principalType): all drop.
	e.InvalidateCache("", "")
	if got := e.CacheSize(); got != 0 {
		t.Fatalf("after InvalidateAll: got %d, want 0", got)
	}
}

// TestEngine_LoadPoliciesErrorSurfaces covers the loader-error path in
// loadPolicies → EvaluateMulti. Without this the only path tested is
// the happy path; a DB outage would surface as nil result + nil error
// from the engine instead of bubbling the loader error.
func TestEngine_LoadPoliciesErrorSurfaces(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	want := errors.New("simulated DB outage")
	e := NewEngine(&errLoader{err: want}, logger)

	res, err := e.Evaluate(context.Background(), "user", "alice", "x:y", "nrn:*", nil)
	if err == nil {
		t.Fatal("expected loader error to surface")
	}
	if !errors.Is(err, want) {
		t.Errorf("expected loader err to be wrapped/returned, got: %v", err)
	}
	if res != nil {
		t.Errorf("result should be nil on loader err, got: %+v", res)
	}
}
