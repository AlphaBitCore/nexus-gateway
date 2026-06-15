package quota

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
)

// TestEngine_Check_UsageCacheError_FailsOpenAndLogs verifies F-0004 on the
// Check path: when the usage-cache (Redis) read fails during Check, the engine
// skips that level (fail-open) so the request is allowed through, logs the
// error so the outage is observable, and increments the fail-open metric.
// Without this test the error-discard at the GetUsage call site in Check is
// invisible to the test suite and a regression (e.g. re-introducing the "_"
// discard) would go undetected.
func TestEngine_Check_UsageCacheError_FailsOpenAndLogs(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	// Close miniredis so every GetUsage call returns a connection error.
	mr.Close()

	policyCache := NewPolicyCache(nil, testLogger())
	policyCache.policiesByScope["virtual_key"] = []CachedPolicy{
		{ID: "p-check", Scope: "virtual_key", PeriodType: "monthly", CostLimitCents: 1, EnforcementMode: "reject", Priority: 100},
	}
	usageCache := NewUsageCache(rdb, testLogger())

	// Capture log output to assert the error is surfaced (not swallowed).
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Use a real Metrics so the fail-open counter is observable.
	metrics := NewMetrics("test_check_failopen", prometheus.NewRegistry())
	engine := NewEngine(policyCache, usageCache, logger, metrics)

	chain := []CheckLevel{{TargetType: "virtual_key", TargetID: "vk-fo"}}
	meta := &vkauth.VKMeta{ID: "vk-fo", OrganizationID: "org"}

	// Even though the VK is over its 1-cent limit (we can't read the usage),
	// the engine must fail-open and allow the request.
	d := engine.Check(context.Background(), chain,
		CostEstimate{EstimatedInputTokens: 1_000_000, InputPricePM: 10}, meta)

	if !d.Allowed {
		t.Errorf("cache error during Check must fail-open (allow); got Allowed=false, action=%q", d.Action)
	}
	if d.Action != "allow" {
		t.Errorf("fail-open action: got %q, want allow", d.Action)
	}

	// The error must appear in the log so ops can alert on the unenforced window.
	logOut := logBuf.String()
	if !strings.Contains(logOut, "get usage cache") {
		t.Errorf("expected error log for usage-cache failure in Check; got %q", logOut)
	}
}

// TestEngine_VKLimit_UsageCacheError_FailsOpenAndLogs verifies F-0004: when the
// usage-cache (Redis) read fails, VKLimit still returns the resolved limit with
// hasLimit=true (so the usage query is not failed shut), reports current usage
// as 0 (deliberate fail-open), and logs a warn line so the outage is observable
// rather than swallowed silently.
func TestEngine_VKLimit_UsageCacheError_FailsOpenAndLogs(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	// Closing the server makes every subsequent Redis op (GetUsage GET) error.
	mr.Close()

	policyCache := NewPolicyCache(nil, testLogger())
	policyCache.policiesByScope["virtual_key"] = []CachedPolicy{
		{ID: "p-1", Scope: "virtual_key", PeriodType: "monthly", CostLimitCents: 100, EnforcementMode: "reject", Priority: 100},
	}
	usageCache := NewUsageCache(rdb, testLogger())

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	engine := NewEngine(policyCache, usageCache, logger, nil)

	limit, current, period, has := engine.VKLimit(context.Background(),
		&vkauth.VKMeta{ID: "vk-x", OrganizationID: "org"})

	if !has {
		t.Fatalf("usage-cache error must not fail the limit lookup; got has=false")
	}
	if limit != 100 {
		t.Errorf("expected policy limit 100 despite cache error; got %d", limit)
	}
	if current != 0 {
		t.Errorf("fail-open must report current=0 on cache error; got %d", current)
	}
	if period != CurrentPeriodKey("monthly") {
		t.Errorf("expected monthly period key; got %q", period)
	}
	if !strings.Contains(logBuf.String(), "usage-cache read failed") {
		t.Errorf("expected fail-open warn log; got %q", logBuf.String())
	}
}
