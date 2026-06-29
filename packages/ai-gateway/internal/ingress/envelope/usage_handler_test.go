package envelope

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// stubUsageStore is a test double for usageStore.
type stubUsageStore struct {
	rows []store.DailyModelUsage
	err  error
}

func (s *stubUsageStore) GetDailyUsageForVK(_ context.Context, _ string, _, _ time.Time) ([]store.DailyModelUsage, error) {
	return s.rows, s.err
}

// stubVKAuth is a test double for vkAuthenticator.
type stubVKAuth struct {
	meta *vkauth.VKMeta
	err  error
}

func (s *stubVKAuth) Authenticate(_ context.Context, _ *http.Request) (*vkauth.VKMeta, error) {
	return s.meta, s.err
}

func newLogger() *slog.Logger { return slog.Default() }

func decodeErrorBody(t *testing.T, body []byte) (code, message string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("failed to decode error body: %v — raw: %s", err, body)
	}
	return env.Error.Code, env.Error.Message
}

func TestUsageSummaryHandler_AuthFail_Returns401(t *testing.T) {
	auth := &stubVKAuth{err: vkauth.ErrMissing}
	db := &stubUsageStore{}
	h := UsageSummaryHandler(db, auth, nil, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	code, _ := decodeErrorBody(t, w.Body.Bytes())
	if code != "AUTH_INVALID" {
		t.Errorf("error code = %q, want AUTH_INVALID", code)
	}
}

func TestUsageSummaryHandler_DBError_Returns500(t *testing.T) {
	auth := &stubVKAuth{meta: &vkauth.VKMeta{ID: "vk-1"}}
	db := &stubUsageStore{err: errors.New("connection refused")}
	h := UsageSummaryHandler(db, auth, nil, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	code, _ := decodeErrorBody(t, w.Body.Bytes())
	if code != "USAGE_QUERY_FAILED" {
		t.Errorf("error code = %q, want USAGE_QUERY_FAILED", code)
	}
}

func TestUsageSummaryHandler_HappyPath_EmptyRows_Returns200(t *testing.T) {
	auth := &stubVKAuth{meta: &vkauth.VKMeta{ID: "vk-empty"}}
	db := &stubUsageStore{rows: nil}
	h := UsageSummaryHandler(db, auth, nil, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp usageSummaryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.VirtualKeyID != "vk-empty" {
		t.Errorf("virtualKeyId = %q, want vk-empty", resp.VirtualKeyID)
	}
	if resp.Usage.TotalRequests != 0 {
		t.Errorf("totalRequests = %d, want 0", resp.Usage.TotalRequests)
	}
	if resp.Quota != nil {
		t.Errorf("quota block should be nil when no budget or rate limit set")
	}
}

func TestUsageSummaryHandler_HappyPath_WithRows_AggregatesTokens(t *testing.T) {
	auth := &stubVKAuth{meta: &vkauth.VKMeta{ID: "vk-42"}}
	db := &stubUsageStore{rows: []store.DailyModelUsage{
		{
			Day:       time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			ModelName: "gpt-4o", ProviderName: "openai",
			Requests: 100, PromptTokens: 50000, CompletionTokens: 20000, TotalTokens: 70000, CostUsd: 1.50,
		},
		{
			Day:       time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC),
			ModelName: "claude-sonnet", ProviderName: "anthropic",
			Requests: 50, PromptTokens: 10000, CompletionTokens: 5000, TotalTokens: 15000, CostUsd: 0.30,
		},
	}}
	h := UsageSummaryHandler(db, auth, nil, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp usageSummaryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Usage.TotalRequests != 150 {
		t.Errorf("totalRequests = %d, want 150", resp.Usage.TotalRequests)
	}
	if resp.Usage.PromptTokens != 60000 {
		t.Errorf("promptTokens = %d, want 60000", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 25000 {
		t.Errorf("completionTokens = %d, want 25000", resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != 85000 {
		t.Errorf("totalTokens = %d, want 85000", resp.Usage.TotalTokens)
	}
	if resp.Usage.EstimatedCostUsd != 1.80 {
		t.Errorf("estimatedCostUsd = %f, want 1.80", resp.Usage.EstimatedCostUsd)
	}
	if resp.PeriodType != "monthly" {
		t.Errorf("periodType = %q, want monthly", resp.PeriodType)
	}
}

func TestUsageSummaryHandler_RateLimitOnly_BuildsQuotaBlockWithoutBudget(t *testing.T) {
	rpm := 60
	auth := &stubVKAuth{meta: &vkauth.VKMeta{ID: "vk-rateonly", RateLimitRpm: &rpm}}
	db := &stubUsageStore{}
	h := UsageSummaryHandler(db, auth, nil, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	h.ServeHTTP(w, r)

	var resp usageSummaryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Quota == nil {
		t.Fatal("quota block expected when rate limit is set")
	}
	if resp.Quota.EnforcementMode != "none" {
		t.Errorf("enforcementMode = %q, want none", resp.Quota.EnforcementMode)
	}
	if resp.Quota.RateLimitRpm == nil || *resp.Quota.RateLimitRpm != 60 {
		t.Errorf("rateLimitRpm = %v, want 60", resp.Quota.RateLimitRpm)
	}
}

func TestUsageSummaryHandler_ContentTypeIsJSON(t *testing.T) {
	auth := &stubVKAuth{meta: &vkauth.VKMeta{ID: "vk-ct"}}
	db := &stubUsageStore{}
	h := UsageSummaryHandler(db, auth, nil, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	h.ServeHTTP(w, r)

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestUsageDailyHandler_AuthFail_Returns401(t *testing.T) {
	auth := &stubVKAuth{err: vkauth.ErrInvalid}
	db := &stubUsageStore{}
	h := UsageDailyHandler(db, auth, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	code, _ := decodeErrorBody(t, w.Body.Bytes())
	if code != "AUTH_INVALID" {
		t.Errorf("error code = %q, want AUTH_INVALID", code)
	}
}

func TestUsageDailyHandler_DBError_Returns500(t *testing.T) {
	auth := &stubVKAuth{meta: &vkauth.VKMeta{ID: "vk-dberr"}}
	db := &stubUsageStore{err: errors.New("query timeout")}
	h := UsageDailyHandler(db, auth, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	code, _ := decodeErrorBody(t, w.Body.Bytes())
	if code != "USAGE_QUERY_FAILED" {
		t.Errorf("error code = %q, want USAGE_QUERY_FAILED", code)
	}
}

func TestUsageDailyHandler_HappyPath_EmptyRows_Returns200WithEmptyDaily(t *testing.T) {
	auth := &stubVKAuth{meta: &vkauth.VKMeta{ID: "vk-daily-empty"}}
	db := &stubUsageStore{rows: nil}
	h := UsageDailyHandler(db, auth, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp dailyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.VirtualKeyID != "vk-daily-empty" {
		t.Errorf("virtualKeyId = %q, want vk-daily-empty", resp.VirtualKeyID)
	}
	if len(resp.Daily) != 0 {
		t.Errorf("daily len = %d, want 0", len(resp.Daily))
	}
	if resp.Totals.Requests != 0 {
		t.Errorf("totals.requests = %d, want 0", resp.Totals.Requests)
	}
}

func TestUsageDailyHandler_HappyPath_WithRows_AggregatesCorrectly(t *testing.T) {
	auth := &stubVKAuth{meta: &vkauth.VKMeta{ID: "vk-daily-data"}}
	db := &stubUsageStore{rows: []store.DailyModelUsage{
		{
			Day:       time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC),
			ModelName: "gpt-4o", ProviderName: "openai",
			Requests: 20, PromptTokens: 8000, CompletionTokens: 3000, TotalTokens: 11000, CostUsd: 0.22,
		},
		{
			Day:       time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC),
			ModelName: "claude-sonnet", ProviderName: "anthropic",
			Requests: 10, PromptTokens: 2000, CompletionTokens: 1000, TotalTokens: 3000, CostUsd: 0.08,
		},
	}}
	h := UsageDailyHandler(db, auth, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily?startDate=2026-04-01&endDate=2026-04-10", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — body: %s", w.Code, w.Body.Bytes())
	}
	var resp dailyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StartDate != "2026-04-01" {
		t.Errorf("startDate = %q, want 2026-04-01", resp.StartDate)
	}
	if resp.EndDate != "2026-04-10" {
		t.Errorf("endDate = %q, want 2026-04-10", resp.EndDate)
	}
	if len(resp.Daily) != 1 {
		t.Fatalf("daily len = %d, want 1", len(resp.Daily))
	}
	day := resp.Daily[0]
	if day.Date != "2026-04-10" {
		t.Errorf("daily[0].date = %q, want 2026-04-10", day.Date)
	}
	if day.Requests != 30 {
		t.Errorf("daily[0].requests = %d, want 30", day.Requests)
	}
	if len(day.Models) != 2 {
		t.Errorf("daily[0].models len = %d, want 2", len(day.Models))
	}
	if resp.Totals.Requests != 30 {
		t.Errorf("totals.requests = %d, want 30", resp.Totals.Requests)
	}
}

func TestUsageDailyHandler_InvalidStartDate_Returns400(t *testing.T) {
	auth := &stubVKAuth{meta: &vkauth.VKMeta{ID: "vk-parse"}}
	db := &stubUsageStore{}
	h := UsageDailyHandler(db, auth, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily?startDate=not-a-date", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	code, _ := decodeErrorBody(t, w.Body.Bytes())
	if code != "USAGE_INVALID_DATE" {
		t.Errorf("error code = %q, want USAGE_INVALID_DATE", code)
	}
}

func TestUsageDailyHandler_InvalidEndDate_Returns400(t *testing.T) {
	auth := &stubVKAuth{meta: &vkauth.VKMeta{ID: "vk-parse2"}}
	db := &stubUsageStore{}
	h := UsageDailyHandler(db, auth, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily?endDate=2026/04/01", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	code, _ := decodeErrorBody(t, w.Body.Bytes())
	if code != "USAGE_INVALID_DATE" {
		t.Errorf("error code = %q, want USAGE_INVALID_DATE", code)
	}
}

func TestUsageDailyHandler_EndBeforeStart_Returns400(t *testing.T) {
	auth := &stubVKAuth{meta: &vkauth.VKMeta{ID: "vk-range"}}
	db := &stubUsageStore{}
	h := UsageDailyHandler(db, auth, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily?startDate=2026-04-10&endDate=2026-04-01", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	code, msg := decodeErrorBody(t, w.Body.Bytes())
	if code != "USAGE_INVALID_DATE" {
		t.Errorf("error code = %q, want USAGE_INVALID_DATE", code)
	}
	if msg == "" {
		t.Error("error message must not be empty")
	}
}

func TestUsageDailyHandler_RangeTooLarge_Returns400(t *testing.T) {
	auth := &stubVKAuth{meta: &vkauth.VKMeta{ID: "vk-bigrange"}}
	db := &stubUsageStore{}
	h := UsageDailyHandler(db, auth, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily?startDate=2026-01-01&endDate=2026-04-17", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	code, _ := decodeErrorBody(t, w.Body.Bytes())
	if code != "USAGE_RANGE_TOO_LARGE" {
		t.Errorf("error code = %q, want USAGE_RANGE_TOO_LARGE", code)
	}
}

func TestUsageDailyHandler_ContentTypeIsJSON(t *testing.T) {
	auth := &stubVKAuth{meta: &vkauth.VKMeta{ID: "vk-ct2"}}
	db := &stubUsageStore{}
	h := UsageDailyHandler(db, auth, newLogger())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily", nil)
	h.ServeHTTP(w, r)

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}
