package wiring

import (
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// sharedSemanticDeps holds the results from both InitSemantic test scenarios.
// freshness.NewDetector uses promauto.With(prometheus.DefaultRegisterer) which
// panics on duplicate registration — so each distinct namespace may only be
// registered once per process. We build one degraded and one full-featured
// instance, each under a unique namespace, guarded by sync.Once.
var (
	semanticDegradedOnce sync.Once
	semanticDegradedDeps SemanticDeps

	semanticRedisDeps SemanticDeps
	semanticRedisOnce sync.Once
)

func getSemanticDegradedDeps() SemanticDeps {
	semanticDegradedOnce.Do(func() {
		semanticDegradedDeps = InitSemantic(nil, nil, "nexus_test_degraded", discardLogger())
	})
	return semanticDegradedDeps
}

func getSemanticRedisDeps(t *testing.T) SemanticDeps {
	t.Helper()
	semanticRedisOnce.Do(func() {
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		semanticRedisDeps = InitSemantic(rdb, nil, "nexus_test_redis", discardLogger())
	})
	return semanticRedisDeps
}

// TestInitSemantic_nilRdb verifies that a nil Redis client returns a
// degraded SemanticDeps (Reader and Writer nil, ConfigCache non-nil).
func TestInitSemantic_nilRdb(t *testing.T) {
	deps := getSemanticDegradedDeps()
	if deps.ConfigCache == nil {
		t.Error("expected non-nil ConfigCache even with nil rdb")
	}
	if deps.Reader != nil {
		t.Error("expected nil Reader when rdb=nil (degraded mode)")
	}
	if deps.Writer != nil {
		t.Error("expected nil Writer when rdb=nil (degraded mode)")
	}
	if deps.BudgetTracker != nil {
		t.Error("expected nil BudgetTracker when rdb=nil (degraded mode)")
	}
}

// TestInitSemantic_withRedisClient verifies that a *redis.Client yields
// a fully populated SemanticDeps (Reader and Writer non-nil).
func TestInitSemantic_withRedisClient(t *testing.T) {
	deps := getSemanticRedisDeps(t)
	if deps.ConfigCache == nil {
		t.Error("expected non-nil ConfigCache")
	}
	if deps.Reader == nil {
		t.Error("expected non-nil Reader with valid *redis.Client")
	}
	if deps.Writer == nil {
		t.Error("expected non-nil Writer with valid *redis.Client")
	}
	if deps.BudgetTracker == nil {
		t.Error("expected non-nil BudgetTracker with valid *redis.Client")
	}
	if deps.IndexLifecycle == nil {
		t.Error("expected non-nil IndexLifecycle")
	}
}
