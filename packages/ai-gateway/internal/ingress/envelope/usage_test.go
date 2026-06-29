package envelope

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
)

func TestParseDateRange(t *testing.T) {
	now := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		query     string
		wantStart string
		wantEnd   string
		wantErr   string
	}{
		{
			name:      "defaults to last 30 days",
			query:     "",
			wantStart: "2026-03-18",
			wantEnd:   "2026-04-17",
		},
		{
			name:      "custom start and end",
			query:     "startDate=2026-04-01&endDate=2026-04-10",
			wantStart: "2026-04-01",
			wantEnd:   "2026-04-10",
		},
		{
			name:      "only startDate",
			query:     "startDate=2026-04-10",
			wantStart: "2026-04-10",
			wantEnd:   "2026-04-17",
		},
		{
			name:      "only endDate",
			query:     "endDate=2026-04-10",
			wantStart: "2026-03-18",
			wantEnd:   "2026-04-10",
		},
		{
			name:    "invalid startDate format",
			query:   "startDate=not-a-date",
			wantErr: "USAGE_INVALID_DATE",
		},
		{
			name:    "invalid endDate format",
			query:   "endDate=2026/04/01",
			wantErr: "USAGE_INVALID_DATE",
		},
		{
			name:    "end before start",
			query:   "startDate=2026-04-10&endDate=2026-04-01",
			wantErr: "USAGE_INVALID_DATE",
		},
		{
			name:    "range exceeds 90 days",
			query:   "startDate=2026-01-01&endDate=2026-04-17",
			wantErr: "USAGE_RANGE_TOO_LARGE",
		},
		{
			name:      "exactly 90 days is OK",
			query:     "startDate=2026-01-18&endDate=2026-04-17",
			wantStart: "2026-01-18",
			wantEnd:   "2026-04-17",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily?"+tt.query, nil)
			start, end, err := parseDateRange(r, now)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %q, got nil", tt.wantErr)
				}
				if err.code != tt.wantErr {
					t.Errorf("error code = %q, want %q", err.code, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := start.Format("2006-01-02"); got != tt.wantStart {
				t.Errorf("start = %q, want %q", got, tt.wantStart)
			}
			if got := end.Format("2006-01-02"); got != tt.wantEnd {
				t.Errorf("end = %q, want %q", got, tt.wantEnd)
			}
		})
	}
}

func TestParsePeriodStart(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"2026-04", "2026-04-01"},
		{"2026-01", "2026-01-01"},
		{"2025-12", "2025-12-01"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parsePeriodStart(tt.input)
			if got.Format("2006-01-02") != tt.want {
				t.Errorf("parsePeriodStart(%q) = %v, want %v", tt.input, got.Format("2006-01-02"), tt.want)
			}
		})
	}
}

func TestBuildDailyResponse(t *testing.T) {
	start := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)

	rows := []store.DailyModelUsage{
		{Day: time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC), ModelName: "gpt-4o", ProviderName: "openai", Requests: 50, PromptTokens: 18000, CompletionTokens: 7200, TotalTokens: 25200, CostUsd: 0.52},
		{Day: time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC), ModelName: "claude-sonnet", ProviderName: "anthropic", Requests: 35, PromptTokens: 6000, CompletionTokens: 2600, TotalTokens: 8600, CostUsd: 0.16},
		{Day: time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC), ModelName: "gpt-4o", ProviderName: "openai", Requests: 100, PromptTokens: 40000, CompletionTokens: 15000, TotalTokens: 55000, CostUsd: 1.10},
	}

	resp := buildDailyResponse("vk-123", start, end, rows)

	if resp.VirtualKeyID != "vk-123" {
		t.Errorf("virtualKeyId = %q, want %q", resp.VirtualKeyID, "vk-123")
	}
	if resp.StartDate != "2026-04-15" {
		t.Errorf("startDate = %q, want %q", resp.StartDate, "2026-04-15")
	}
	if len(resp.Daily) != 2 {
		t.Fatalf("daily len = %d, want 2", len(resp.Daily))
	}

	day1 := resp.Daily[0]
	if day1.Date != "2026-04-17" {
		t.Errorf("daily[0].date = %q, want %q", day1.Date, "2026-04-17")
	}
	if day1.Requests != 85 {
		t.Errorf("daily[0].requests = %d, want 85", day1.Requests)
	}
	if len(day1.Models) != 2 {
		t.Fatalf("daily[0].models len = %d, want 2", len(day1.Models))
	}
	if day1.Models[0].Model != "gpt-4o" {
		t.Errorf("daily[0].models[0].model = %q, want %q", day1.Models[0].Model, "gpt-4o")
	}

	day2 := resp.Daily[1]
	if day2.Date != "2026-04-16" {
		t.Errorf("daily[1].date = %q, want %q", day2.Date, "2026-04-16")
	}
	if day2.Requests != 100 {
		t.Errorf("daily[1].requests = %d, want 100", day2.Requests)
	}

	if resp.Totals.Requests != 185 {
		t.Errorf("totals.requests = %d, want 185", resp.Totals.Requests)
	}
	if resp.Totals.PromptTokens != 64000 {
		t.Errorf("totals.promptTokens = %d, want 64000", resp.Totals.PromptTokens)
	}

	// Empty rows → empty daily array.
	emptyResp := buildDailyResponse("vk-456", start, end, nil)
	if len(emptyResp.Daily) != 0 {
		t.Errorf("empty daily len = %d, want 0", len(emptyResp.Daily))
	}
	if emptyResp.Totals.Requests != 0 {
		t.Errorf("empty totals.requests = %d, want 0", emptyResp.Totals.Requests)
	}
}

func TestUsageSummaryHandler_NoDB(t *testing.T) {
	h := UsageSummaryHandler(nil, nil, nil, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

func TestUsageDailyHandler_NoDB(t *testing.T) {
	h := UsageDailyHandler(nil, nil, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// fakeVKLookupEnvelope is a minimal in-memory VKLookup so we can construct a
// real *vkauth.Authenticator. Maps HMAC-SHA256(key) → VirtualKey row.
type fakeVKLookupEnvelope struct {
	keys map[string]*store.VirtualKey
}

func (f *fakeVKLookupEnvelope) GetVirtualKeyByHash(_ context.Context, keyHash string) (*store.VirtualKey, error) {
	if vk, ok := f.keys[keyHash]; ok {
		return vk, nil
	}
	return nil, nil // miss
}

func hmacHashVKEnvelope(raw, secret string) string {
	// the authenticator hashes VKs under the HKDF-derived VK-domain
	// sub-key, not the raw master — mirror that so the seeded lookup hash matches.
	sub := keyderive.DeriveSubkey([]byte(secret), keyderive.ClassAPIKeyVirtualKey)
	mac := hmac.New(sha256.New, sub[:])
	_, _ = mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}

// TestUsageSummaryHandler_AuthMissing verifies that a missing/invalid virtual
// key returns HTTP 401 with AUTH_INVALID in the body.
func TestUsageSummaryHandler_AuthMissing(t *testing.T) {
	mock, dbErr := pgxmock.NewPool()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)

	authn := vkauth.NewAuthenticator(&fakeVKLookupEnvelope{}, vkKeyring("test-secret"), slog.Default())
	h := UsageSummaryHandler(db, authn, nil, slog.Default())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	h(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "AUTH_INVALID") {
		t.Errorf("body missing AUTH_INVALID: %s", w.Body.String())
	}
}

// TestUsageDailyHandler_AuthMissing verifies that a missing virtual key
// returns HTTP 401 for the daily endpoint.
func TestUsageDailyHandler_AuthMissing(t *testing.T) {
	mock, dbErr := pgxmock.NewPool()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)

	authn := vkauth.NewAuthenticator(&fakeVKLookupEnvelope{}, vkKeyring("test-secret"), slog.Default())
	h := UsageDailyHandler(db, authn, slog.Default())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily", nil)
	h(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", w.Code)
	}
}

// TestUsageDailyHandler_InvalidDate verifies that a malformed startDate
// query parameter returns HTTP 400 with USAGE_INVALID_DATE.
func TestUsageDailyHandler_InvalidDate(t *testing.T) {
	mock, dbErr := pgxmock.NewPool()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)

	hmacSecret := "test-secret-key"
	vkKey := "nx-12345678901234567890"
	authn := vkauth.NewAuthenticator(&fakeVKLookupEnvelope{
		keys: map[string]*store.VirtualKey{
			hmacHashVKEnvelope(vkKey, hmacSecret): {ID: "vk-1", Name: "test-vk", Enabled: true},
		},
	}, vkKeyring(hmacSecret), slog.Default())

	h := UsageDailyHandler(db, authn, slog.Default())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily?startDate=bad-date", nil)
	r.Header.Set("Authorization", "Bearer "+vkKey)
	h(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "USAGE_INVALID_DATE") {
		t.Errorf("body: %s", w.Body.String())
	}
}

// TestUsageDailyHandler_DBQueryError verifies that a DB failure returns
// HTTP 500 with USAGE_QUERY_FAILED when the authenticated VK is valid.
func TestUsageDailyHandler_DBQueryError(t *testing.T) {
	mock, dbErr := pgxmock.NewPool()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)
	mock.ExpectQuery("FROM traffic_event").WillReturnError(errors.New("db down"))

	hmacSecret := "test-secret-key"
	vkKey := "nx-12345678901234567890"
	authn := vkauth.NewAuthenticator(&fakeVKLookupEnvelope{
		keys: map[string]*store.VirtualKey{
			hmacHashVKEnvelope(vkKey, hmacSecret): {ID: "vk-1", Name: "test-vk", Enabled: true},
		},
	}, vkKeyring(hmacSecret), slog.Default())

	h := UsageDailyHandler(db, authn, slog.Default())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily", nil)
	r.Header.Set("Authorization", "Bearer "+vkKey)
	h(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "USAGE_QUERY_FAILED") {
		t.Errorf("body: %s", w.Body.String())
	}
}

// TestUsageDailyHandler_HappyPath verifies that a successful daily query
// returns HTTP 200 with virtualKeyId and totals populated.
func TestUsageDailyHandler_HappyPath(t *testing.T) {
	mock, dbErr := pgxmock.NewPool()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM traffic_event").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnRows(
		pgxmock.NewRows([]string{"day", "model_name", "provider_name", "requests", "prompt_tokens", "completion_tokens", "total_tokens", "cost_usd"}).
			AddRow(day, "gpt-4o", "openai", int64(10), int64(100), int64(50), int64(150), 0.25),
	)

	hmacSecret := "test-secret-key"
	vkKey := "nx-12345678901234567890"
	authn := vkauth.NewAuthenticator(&fakeVKLookupEnvelope{
		keys: map[string]*store.VirtualKey{
			hmacHashVKEnvelope(vkKey, hmacSecret): {ID: "vk-1", Name: "test-vk", Enabled: true},
		},
	}, vkKeyring(hmacSecret), slog.Default())

	h := UsageDailyHandler(db, authn, slog.Default())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage/daily", nil)
	r.Header.Set("Authorization", "Bearer "+vkKey)
	h(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := gjson.GetBytes(w.Body.Bytes(), "virtualKeyId").String(); got != "vk-1" {
		t.Errorf("virtualKeyId=%q", got)
	}
	if got := gjson.GetBytes(w.Body.Bytes(), "totals.requests").Int(); got != 10 {
		t.Errorf("totals.requests=%d", got)
	}
}

// TestUsageSummaryHandler_DBQueryError verifies that a DB failure returns
// HTTP 500 with USAGE_QUERY_FAILED on the summary endpoint.
func TestUsageSummaryHandler_DBQueryError(t *testing.T) {
	mock, dbErr := pgxmock.NewPool()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)
	mock.ExpectQuery("FROM traffic_event").WillReturnError(errors.New("db down"))

	hmacSecret := "test-secret-key"
	vkKey := "nx-12345678901234567890"
	authn := vkauth.NewAuthenticator(&fakeVKLookupEnvelope{
		keys: map[string]*store.VirtualKey{
			hmacHashVKEnvelope(vkKey, hmacSecret): {ID: "vk-1", Name: "test-vk", Enabled: true},
		},
	}, vkKeyring(hmacSecret), slog.Default())

	h := UsageSummaryHandler(db, authn, nil, slog.Default())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	r.Header.Set("Authorization", "Bearer "+vkKey)
	h(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", w.Code)
	}
}

// TestUsageSummaryHandler_HappyPath_NoQuota verifies that the summary
// handler returns HTTP 200 with correct usage totals when no quota is set.
func TestUsageSummaryHandler_HappyPath_NoQuota(t *testing.T) {
	mock, dbErr := pgxmock.NewPool()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	defer mock.Close()
	db := store.NewWithPgxPool(mock)
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM traffic_event").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnRows(
		pgxmock.NewRows([]string{"day", "model_name", "provider_name", "requests", "prompt_tokens", "completion_tokens", "total_tokens", "cost_usd"}).
			AddRow(day, "gpt-4o", "openai", int64(5), int64(50), int64(20), int64(70), 0.10),
	)

	hmacSecret := "test-secret-key"
	vkKey := "nx-12345678901234567890"
	authn := vkauth.NewAuthenticator(&fakeVKLookupEnvelope{
		keys: map[string]*store.VirtualKey{
			hmacHashVKEnvelope(vkKey, hmacSecret): {ID: "vk-1", Name: "test-vk", Enabled: true},
		},
	}, vkKeyring(hmacSecret), slog.Default())

	h := UsageSummaryHandler(db, authn, nil, slog.Default())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	r.Header.Set("Authorization", "Bearer "+vkKey)
	h(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := gjson.GetBytes(w.Body.Bytes(), "virtualKeyId").String(); got != "vk-1" {
		t.Errorf("virtualKeyId=%q", got)
	}
	if got := gjson.GetBytes(w.Body.Bytes(), "usage.totalRequests").Int(); got != 5 {
		t.Errorf("totalRequests=%d", got)
	}
}
