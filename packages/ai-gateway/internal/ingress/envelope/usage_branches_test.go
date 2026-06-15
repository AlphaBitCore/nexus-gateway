// Package envelope — usage_gap_test.go covers branches not reached by usage_test.go.
//
// Named failure modes:
//   - UsageSummaryHandler with nil DB → 500
//   - UsageDailyHandler with nil DB → 500
//   - writeDetailedError with non-empty hint → hint field present
//   - writeDetailedError with empty hint → hint field absent
//   - parsePeriodStart with valid key → correct date
//   - parsePeriodStart with invalid key → falls back to current month
//   - UsageSummaryHandler with quotaEngine + VKLimit → quotaBlock populated
//   - UsageSummaryHandler with quotaEngine + usage > limit → remaining clamped to 0
package envelope

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
)

var testLogger = slog.Default()

func TestUsageSummaryHandler_nilDB_returns500(t *testing.T) {
	h := UsageSummaryHandler(nil, nil, nil, testLogger)
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "USAGE_QUERY_FAILED" {
		t.Errorf("code: got %v", errObj["code"])
	}
}

func TestUsageDailyHandler_nilDB_returns500(t *testing.T) {
	h := UsageDailyHandler(nil, nil, testLogger)
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily", nil)
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", w.Code)
	}
}

func TestWriteDetailedError_withHint_hintFieldPresent(t *testing.T) {
	w := httptest.NewRecorder()
	writeDetailedError(w, http.StatusBadRequest, "BAD_INPUT", "bad input", "Try again")
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["hint"] != "Try again" {
		t.Errorf("hint: got %v", errObj["hint"])
	}
}

func TestWriteDetailedError_withoutHint_hintFieldAbsent(t *testing.T) {
	w := httptest.NewRecorder()
	writeDetailedError(w, http.StatusInternalServerError, "SERVER_ERR", "server error", "")
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if _, ok := errObj["hint"]; ok {
		t.Error("hint field should be absent when empty")
	}
}

func TestParsePeriodStart_validKey_correctDate(t *testing.T) {
	got := parsePeriodStart("2026-04")
	want := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParsePeriodStart_invalidKey_currentMonthFallback(t *testing.T) {
	got := parsePeriodStart("not-a-period")
	// Should return current month's first day without panic.
	if got.Day() != 1 {
		t.Errorf("expected day=1 for current month fallback, got %v", got)
	}
	if got.Hour() != 0 || got.Minute() != 0 {
		t.Errorf("expected midnight, got %v", got)
	}
}

// newTestQuotaEngine builds an in-memory quota.Engine pre-populated with a
// single virtual_key-scope policy + an injected usage figure. limitCents
// drives the policy's CostLimitCents; usageCents drives the UsageCache row
// for vk-id under the monthly period.
func newTestQuotaEngine(t *testing.T, vkID string, limitCents, usageCents int64) *quota.Engine {
	t.Helper()
	pc := quota.NewPolicyCache(nil, slog.Default())
	pc.SetPoliciesForTest(map[string][]quota.CachedPolicy{
		"virtual_key": {{
			ID: "p-test", Scope: "virtual_key", PeriodType: "monthly",
			CostLimitCents: limitCents, EnforcementMode: "reject", Priority: 100,
		}},
	})
	uc := quota.NewUsageCache(nil, slog.Default())
	uc.SetUsageForTest("virtual_key", vkID, quota.CurrentPeriodKey("monthly"), usageCents)
	return quota.NewEngine(pc, uc, slog.Default(), nil)
}

// TestUsageSummaryHandler_QuotaEngine_PopulatesUsageAndQuotaBlock covers the
// happy-path quotaEngine branches (lines 119-126 and 158-175 of usage.go):
// EstimatedCostUsd from the Redis usage cache + a quotaBlock built from a
// resolved VKLimit. Asserts positive remaining stays positive.
func TestUsageSummaryHandler_QuotaEngine_PopulatesUsageAndQuotaBlock(t *testing.T) {
	const vkID = "vk-quota-happy"
	engine := newTestQuotaEngine(t, vkID, 10000, 2500) // $100 limit, $25 used
	auth := &stubVKAuth{meta: &vkauth.VKMeta{ID: vkID}}
	db := &stubUsageStore{}
	h := UsageSummaryHandler(db, auth, engine, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — body: %s", w.Code, w.Body.Bytes())
	}
	var resp usageSummaryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Usage.EstimatedCostUsd != 25.0 {
		t.Errorf("EstimatedCostUsd = %v, want 25.0 (from Redis cache)", resp.Usage.EstimatedCostUsd)
	}
	if resp.Quota == nil {
		t.Fatal("quota block must be present when VKLimit resolves")
	}
	if resp.Quota.LimitUsd != 100.0 {
		t.Errorf("LimitUsd = %v, want 100.0", resp.Quota.LimitUsd)
	}
	if resp.Quota.UsedUsd != 25.0 {
		t.Errorf("UsedUsd = %v, want 25.0", resp.Quota.UsedUsd)
	}
	if resp.Quota.RemainingUsd != 75.0 {
		t.Errorf("RemainingUsd = %v, want 75.0", resp.Quota.RemainingUsd)
	}
	if resp.Quota.EnforcementMode != "reject" {
		t.Errorf("EnforcementMode = %q, want reject", resp.Quota.EnforcementMode)
	}
}

// TestUsageSummaryHandler_QuotaEngine_RemainingClampedToZero covers the
// `if remaining < 0 { remaining = 0 }` branch (line 163-165 of usage.go):
// over-budget VKs must report RemainingUsd = 0, not a negative value.
func TestUsageSummaryHandler_QuotaEngine_RemainingClampedToZero(t *testing.T) {
	const vkID = "vk-over-budget"
	engine := newTestQuotaEngine(t, vkID, 1000, 5000) // $10 limit, $50 used → -$40 raw
	auth := &stubVKAuth{meta: &vkauth.VKMeta{ID: vkID}}
	db := &stubUsageStore{}
	h := UsageSummaryHandler(db, auth, engine, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp usageSummaryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Quota == nil {
		t.Fatal("quota block must be present")
	}
	if resp.Quota.UsedUsd != 50.0 {
		t.Errorf("UsedUsd = %v, want 50.0", resp.Quota.UsedUsd)
	}
	if resp.Quota.RemainingUsd != 0 {
		t.Errorf("RemainingUsd = %v, want 0 (clamped from negative)", resp.Quota.RemainingUsd)
	}
}
