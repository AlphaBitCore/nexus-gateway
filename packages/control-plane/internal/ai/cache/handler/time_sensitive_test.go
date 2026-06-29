package cache

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

// Stub TimeSensitiveStore

// stubTSStore is an in-memory TimeSensitiveStore double for testing.
type stubTSStore struct {
	mu        sync.Mutex
	blob      configstore.TimeSensitiveOverridesBlob
	getErr    error
	saveErr   error
	saveCalls int
}

func (s *stubTSStore) GetOverrides(_ context.Context) (configstore.TimeSensitiveOverridesBlob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return configstore.TimeSensitiveOverridesBlob{}, s.getErr
	}
	return s.blob, nil
}

func (s *stubTSStore) SaveOverrides(_ context.Context, blob configstore.TimeSensitiveOverridesBlob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.blob = blob
	s.saveCalls++
	return nil
}

// newHandlerWithTSStore constructs a Handler with the given tsStore wired in.
// A discarding logger is provided so error paths (e.g. store failures) do not
// panic on a nil *slog.Logger.
func newHandlerWithTSStore(ts TimeSensitiveStore) *Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := newWithPool(nil, nil, nil, logger)
	h.tsStore = ts
	return h
}

// seededTSStore returns a stub store prepopulated with a small fixture
// (weather + stock-price). Used by tests that need an existing rule list
// to mutate, since there are no Go-side defaults — seed.ts owns the real
// production defaults.
func seededTSStore() *stubTSStore {
	return &stubTSStore{
		blob: configstore.TimeSensitiveOverridesBlob{
			Rules: []configstore.TimeSensitiveOverrideRule{
				{ID: "time-current", Keywords: []string{"today", "现在"}, RequireQuestionMark: false, RequireEntity: false, Languages: []string{"en", "zh"}, Enabled: true},
				{ID: "stock-price", Keywords: []string{"stock price", "share price"}, RequireQuestionMark: false, RequireEntity: false, Languages: []string{"en"}, Enabled: true},
				{ID: "exchange-rate", Keywords: []string{"exchange rate"}, RequireQuestionMark: false, RequireEntity: false, Languages: []string{"en"}, Enabled: true},
				{ID: "weather", Keywords: []string{"weather", "temperature"}, RequireQuestionMark: false, RequireEntity: false, Languages: []string{"en", "zh"}, Enabled: true},
			},
		},
	}
}

func TestGetTimeSensitivePatterns_returnsBlob(t *testing.T) {
	h := newHandlerWithTSStore(seededTSStore())

	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/time-sensitive-patterns", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	if err := h.GetTimeSensitivePatterns(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp timeSensitivePatternsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Patterns) == 0 {
		t.Error("expected non-empty patterns list (fixture has 4 rules)")
	}
	if resp.Source != "db" {
		t.Errorf("expected source=db, got %q", resp.Source)
	}
	idSet := make(map[string]bool)
	for _, p := range resp.Patterns {
		idSet[p.ID] = true
	}
	for _, want := range []string{"time-current", "stock-price", "exchange-rate", "weather"} {
		if !idSet[want] {
			t.Errorf("expected fixture rule %q to be present", want)
		}
	}
}

// TestGetTimeSensitivePatterns_nilStoreReturnsEmpty verifies that with no
// tsStore wired in (e.g. dev mode without DB), GET returns an empty list
// rather than resurrecting hardcoded defaults.
func TestGetTimeSensitivePatterns_nilStoreReturnsEmpty(t *testing.T) {
	h := newWithPool(nil, nil, nil, nil) // tsStore=nil

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	if err := h.GetTimeSensitivePatterns(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp timeSensitivePatternsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Patterns) != 0 {
		t.Errorf("expected empty list (no Go-side defaults), got %d", len(resp.Patterns))
	}
}

func TestPutTimeSensitivePattern_unknownID(t *testing.T) {
	h := newWithPool(nil, nil, nil, nil)

	body := `{"id":"nonexistent","keywords":["foo"],"requireQuestionMark":false,"requireEntity":false,"languages":["en"],"enabled":true}`
	req := httptest.NewRequest(http.MethodPut, "/api/admin/cache/time-sensitive-patterns/nonexistent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")
	c.SetParamNames("id")
	c.SetParamValues("nonexistent")

	if err := h.PutTimeSensitivePattern(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestPutTimeSensitivePattern_seedIDNoHub_updatesEnabled(t *testing.T) {
	h := newHandlerWithTSStore(seededTSStore()) // hub=nil: no push, but no error

	body := `{"id":"weather","keywords":["weather","temperature"],"requireQuestionMark":true,"requireEntity":false,"languages":["en","zh"],"enabled":false}`
	req := httptest.NewRequest(http.MethodPut, "/api/admin/cache/time-sensitive-patterns/weather", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")
	c.SetParamNames("id")
	c.SetParamValues("weather")

	if err := h.PutTimeSensitivePattern(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d; body=%s", rec.Code, rec.Body.String())
	}

	var resp timeSensitivePatternsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Find the weather rule in the response and confirm it's disabled.
	found := false
	for _, p := range resp.Patterns {
		if p.ID == "weather" {
			found = true
			if p.Enabled {
				t.Error("expected weather rule to be disabled")
			}
		}
	}
	if !found {
		t.Error("expected weather rule to be in response")
	}
}

func TestPutTimeSensitivePattern_hubPushCalled(t *testing.T) {
	fh := &fakeHub{}
	h := newHandlerWithTSStore(seededTSStore())
	h.hub = fh

	body := `{"id":"weather","keywords":["weather"],"requireQuestionMark":true,"requireEntity":false,"languages":["en"],"enabled":true}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")
	c.SetParamNames("id")
	c.SetParamValues("weather")

	if err := h.PutTimeSensitivePattern(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	fh.mu.Lock()
	defer fh.mu.Unlock()
	if fh.hits != 1 {
		t.Errorf("expected 1 hub call, got %d", fh.hits)
	}
}

func TestPostTimeSensitivePattern_addNewRule(t *testing.T) {
	h := newWithPool(nil, nil, nil, nil)

	body := `{"id":"custom-rule","keywords":["trending","viral"],"requireQuestionMark":true,"requireEntity":false,"languages":["en"],"enabled":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/cache/time-sensitive-patterns", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	if err := h.PostTimeSensitivePattern(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d; body=%s", rec.Code, rec.Body.String())
	}
	var got TimeSensitivePattern
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != "custom-rule" {
		t.Errorf("expected id=custom-rule, got %q", got.ID)
	}
}

func TestPostTimeSensitivePattern_conflictWithExistingID(t *testing.T) {
	h := newHandlerWithTSStore(seededTSStore())

	body := `{"id":"weather","keywords":["rain"],"requireQuestionMark":false,"requireEntity":false,"languages":["en"],"enabled":true}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	if err := h.PostTimeSensitivePattern(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d", rec.Code)
	}
}

func TestPostTimeSensitivePattern_missingKeywords(t *testing.T) {
	h := newWithPool(nil, nil, nil, nil)

	body := `{"id":"new-rule","keywords":[],"requireQuestionMark":false,"requireEntity":false,"languages":["en"],"enabled":true}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	if err := h.PostTimeSensitivePattern(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

// Seed rules can now be deleted (the blob IS the rule list; no hardcoded
// "this rule is immutable" carveout). The prior test that expected 400 on
// seed delete was deleted with the policy.

func TestDeleteTimeSensitivePattern_unknownNonSeedReturns404(t *testing.T) {
	h := newWithPool(nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")
	c.SetParamNames("id")
	c.SetParamValues("my-custom-rule")

	if err := h.DeleteTimeSensitivePattern(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

// TestTimeSensitivePattern (prompt dry-run)

func TestTestTimeSensitivePattern_match(t *testing.T) {
	h := newHandlerWithTSStore(seededTSStore())

	body := `{"prompt":"What is the stock price of AAPL today?","language":"en"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	if err := h.TestTimeSensitivePattern(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp timeSensitiveTestResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Decision != "match" {
		t.Errorf("expected decision=match, got %q", resp.Decision)
	}
	if resp.MatchedRuleID == nil {
		t.Error("expected non-nil matchedRuleId")
	}
}

func TestTestTimeSensitivePattern_noMatch(t *testing.T) {
	h := newWithPool(nil, nil, nil, nil)

	body := `{"prompt":"Explain the history of the Roman Empire.","language":"en"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	if err := h.TestTimeSensitivePattern(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp timeSensitiveTestResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Decision != "no_match" {
		t.Errorf("expected decision=no_match, got %q", resp.Decision)
	}
}

func TestTestTimeSensitivePattern_emptyPrompt(t *testing.T) {
	h := newWithPool(nil, nil, nil, nil)

	body := `{"prompt":"","language":"en"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	if err := h.TestTimeSensitivePattern(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestTestTimeSensitivePattern_malformedJSON(t *testing.T) {
	h := newWithPool(nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	if err := h.TestTimeSensitivePattern(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 on malformed JSON, got %d", rec.Code)
	}
}

func TestTestTimeSensitivePattern_loadErrorReturns500(t *testing.T) {
	// Store returns an error on GetOverrides; the handler surfaces it as 500.
	// There is no fallback to Go-side defaults — seed.ts is the canonical
	// source and a DB outage is a real outage worth alerting on.
	h := newHandlerWithTSStore(&stubTSStore{getErr: errors.New("simulated DB outage")})

	body := `{"prompt":"What is the weather today?","language":"en"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	if err := h.TestTimeSensitivePattern(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on DB outage, got %d", rec.Code)
	}
}

// Helper — entity heuristic

func TestEntityHeuristicCP(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"price is $50", true},
		{"AAPL is up", true},
		{"USD/CNY rate", true},
		{"what year is 2024?", true},
		{"explain this concept", false},
		{"the weather is nice", false},
	}
	for _, tc := range cases {
		got := entityHeuristicCP(tc.text)
		if got != tc.want {
			t.Errorf("entityHeuristicCP(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

// Helper — detectPrompt

// testRules is a small fixture used by detectPrompt unit tests below. The
// canonical default list lives in tools/db-migrate/seed/fixtures/semantic_cache_config.json (time_sensitive_overrides);
// these test rules only need to exercise the matcher itself.
func testRules() []TimeSensitivePattern {
	return []TimeSensitivePattern{
		{ID: "weather", Keywords: []string{"weather", "天气"}, RequireQuestionMark: false, RequireEntity: false, Languages: []string{"en", "zh"}, Enabled: true},
		{ID: "time-current", Keywords: []string{"today", "现在"}, RequireQuestionMark: false, RequireEntity: false, Languages: []string{"en", "zh"}, Enabled: true},
		{ID: "population", Keywords: []string{"人口", "population"}, RequireQuestionMark: false, RequireEntity: false, Languages: []string{"en", "zh"}, Enabled: true},
	}
}

func TestDetectPrompt_weatherMatch(t *testing.T) {
	result := detectPrompt("What is the weather today?", testRules())
	if result.Decision != "match" {
		t.Errorf("expected match, got %q", result.Decision)
	}
	if result.MatchedRuleID == nil || *result.MatchedRuleID == "" {
		t.Error("expected non-empty matched rule id")
	}
}

func TestDetectPrompt_keywordWithoutQuestionMarkStillMatches(t *testing.T) {
	// Product change: rules default RequireQuestionMark=false. Conversational
	// prompts ("现在几点", "Tell me the weather") omit punctuation; treating
	// those as time-sensitive trades a cheap extra upstream call for fresh
	// customer-facing answers.
	result := detectPrompt("Tell me about the weather.", testRules())
	if result.Decision != "match" {
		t.Errorf("expected match on weather keyword without question mark, got %q", result.Decision)
	}
}

func TestDetectPrompt_disabledRuleSkipped(t *testing.T) {
	rules := []TimeSensitivePattern{
		{ID: "disabled-rule", Keywords: []string{"today"}, RequireQuestionMark: true, Enabled: false},
	}
	result := detectPrompt("What is the date today?", rules)
	if result.Decision != "no_match" {
		t.Errorf("expected no_match for disabled rule, got %q", result.Decision)
	}
}

func TestDetectPrompt_requireEntityNotPresent(t *testing.T) {
	rules := []TimeSensitivePattern{
		{ID: "r", Keywords: []string{"price"}, RequireQuestionMark: true, RequireEntity: true, Enabled: true},
	}
	result := detectPrompt("What is the price?", rules)
	// "price" + "?" but no entity (no ticker, currency, number)
	if result.Decision != "no_match" {
		t.Errorf("expected no_match (no entity), got %q", result.Decision)
	}
}

func TestPushTimeSensitiveToHub_nilHub(t *testing.T) {
	h := newWithPool(nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")
	if err := h.pushTimeSensitiveToHub(c, testRules()); err != nil {
		t.Errorf("expected nil error with nil hub, got %v", err)
	}
}

func TestRuleMatches_fullWidthQuestionMark(t *testing.T) {
	rule := TimeSensitivePattern{
		ID:                  "r",
		Keywords:            []string{"今天"},
		RequireQuestionMark: true,
		RequireEntity:       false,
		Enabled:             true,
	}
	matched, _ := ruleMatches(rule, "今天天气怎么样？")
	if !matched {
		t.Error("expected match with full-width question mark ？")
	}
}

// Persistence: GET merges seed + DB overrides

// TestGetTimeSensitivePatterns_returnsStoredBlob verifies GET returns the
// DB-persisted rule list verbatim. seed.ts is the single source of defaults;
// the Handler does no merging.
func TestGetTimeSensitivePatterns_returnsStoredBlob(t *testing.T) {
	ts := &stubTSStore{
		blob: configstore.TimeSensitiveOverridesBlob{
			Rules: []configstore.TimeSensitiveOverrideRule{
				{ID: "weather", Keywords: []string{"rain"}, Enabled: false},
			},
		},
	}
	h := newHandlerWithTSStore(ts)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	if err := h.GetTimeSensitivePatterns(c); err != nil {
		t.Fatalf("GetTimeSensitivePatterns: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp timeSensitivePatternsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Patterns) != 1 {
		t.Fatalf("expected exactly the 1 stored rule; got %d", len(resp.Patterns))
	}
	if resp.Patterns[0].ID != "weather" {
		t.Errorf("expected weather rule; got %q", resp.Patterns[0].ID)
	}
	if resp.Patterns[0].Enabled {
		t.Error("expected weather rule disabled (as stored)")
	}
	if len(resp.Patterns[0].Keywords) != 1 || resp.Patterns[0].Keywords[0] != "rain" {
		t.Errorf("expected keywords [rain]; got %v", resp.Patterns[0].Keywords)
	}
}

// TestGetTimeSensitivePatterns_emptyBlobReturnsEmpty verifies that when the
// DB blob is empty (seed didn't run, or admin deleted everything), GET
// returns an empty patterns list rather than resurrecting hardcoded defaults.
func TestGetTimeSensitivePatterns_emptyBlobReturnsEmpty(t *testing.T) {
	ts := &stubTSStore{} // empty blob
	h := newHandlerWithTSStore(ts)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	if err := h.GetTimeSensitivePatterns(c); err != nil {
		t.Fatalf("GetTimeSensitivePatterns: %v", err)
	}

	var resp timeSensitivePatternsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Source != "db" {
		t.Errorf("expected source=db, got %q", resp.Source)
	}
	if len(resp.Patterns) != 0 {
		t.Errorf("expected empty patterns list (no Go-side defaults); got %d", len(resp.Patterns))
	}
}

// TestGetTimeSensitivePatterns_getStoreError500 surfaces store errors as 500.
func TestGetTimeSensitivePatterns_getStoreError500(t *testing.T) {
	ts := &stubTSStore{getErr: errors.New("db: offline")}
	h := newHandlerWithTSStore(ts)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	if err := h.GetTimeSensitivePatterns(c); err != nil {
		t.Fatalf("unexpected error return: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// Persistence: PUT updates DB blob and pushes merged list to Hub

// TestPutTimeSensitivePattern_persistsOverride verifies that PutTimeSensitivePattern
// writes the updated rule to the DB blob and the hub receives the merged list.
func TestPutTimeSensitivePattern_persistsOverride(t *testing.T) {
	ts := seededTSStore()
	fh := &fakeHub{}
	h := newHandlerWithTSStore(ts)
	h.hub = fh

	body := `{"id":"weather","keywords":["rain","storm"],"requireQuestionMark":true,"requireEntity":false,"languages":["en"],"enabled":false}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")
	c.SetParamNames("id")
	c.SetParamValues("weather")

	if err := h.PutTimeSensitivePattern(c); err != nil {
		t.Fatalf("PutTimeSensitivePattern: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.saveCalls != 1 {
		t.Errorf("expected exactly 1 SaveOverrides call, got %d", ts.saveCalls)
	}
	found := false
	for _, r := range ts.blob.Rules {
		if r.ID == "weather" {
			found = true
			if r.Enabled {
				t.Error("expected weather override to be disabled")
			}
		}
	}
	if !found {
		t.Error("weather rule not in DB blob after PUT")
	}
}

// TestPutTimeSensitivePattern_pageReloadPreservesOverride simulates a page
// reload: PUT weather, then GET — the GET must return the persisted override.
func TestPutTimeSensitivePattern_pageReloadPreservesOverride(t *testing.T) {
	ts := seededTSStore()
	h := newHandlerWithTSStore(ts)

	// PUT: disable weather rule.
	body := `{"id":"weather","keywords":["weather","temperature"],"requireQuestionMark":true,"requireEntity":false,"languages":["en","zh"],"enabled":false}`
	putReq := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	putReq.Header.Set("Content-Type", "application/json")
	putRec := httptest.NewRecorder()
	putCtx := echoContext(putReq, putRec, "admin", "u-1")
	putCtx.SetParamNames("id")
	putCtx.SetParamValues("weather")

	if err := h.PutTimeSensitivePattern(putCtx); err != nil {
		t.Fatalf("PUT: %v", err)
	}
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d", putRec.Code)
	}

	// GET: simulate "page reload" — create a new handler with the SAME tsStore.
	h2 := newHandlerWithTSStore(ts)
	getReq := httptest.NewRequest(http.MethodGet, "/", nil)
	getRec := httptest.NewRecorder()
	getCtx := echoContext(getReq, getRec, "admin", "u-1")

	if err := h2.GetTimeSensitivePatterns(getCtx); err != nil {
		t.Fatalf("GET: %v", err)
	}
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d", getRec.Code)
	}

	var resp timeSensitivePatternsResponse
	if err := json.NewDecoder(getRec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// After "page reload" the override must still be present.
	found := false
	for _, p := range resp.Patterns {
		if p.ID == "weather" {
			found = true
			if p.Enabled {
				t.Error("expected weather to remain disabled after page reload")
			}
		}
	}
	if !found {
		t.Error("weather rule not found after page reload")
	}
	if resp.Source != "db" {
		t.Errorf("expected source=db after override, got %q", resp.Source)
	}
}

// Persistence: POST adds new admin rule to DB blob

// TestPostTimeSensitivePattern_persistsNewRule verifies POST saves the new
// rule to the DB blob.
func TestPostTimeSensitivePattern_persistsNewRule(t *testing.T) {
	ts := &stubTSStore{}
	h := newHandlerWithTSStore(ts)

	body := `{"id":"custom-rule","keywords":["trending","viral"],"requireQuestionMark":true,"requireEntity":false,"languages":["en"],"enabled":true}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	if err := h.PostTimeSensitivePattern(c); err != nil {
		t.Fatalf("PostTimeSensitivePattern: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}

	// saveCalls ≥ 1: lazy-bootstrap (if blob was empty) + the POST itself.
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.saveCalls < 1 {
		t.Errorf("expected at least 1 SaveOverrides call, got %d", ts.saveCalls)
	}
	found := false
	for _, r := range ts.blob.Rules {
		if r.ID == "custom-rule" {
			found = true
		}
	}
	if !found {
		t.Error("custom-rule not in DB blob after POST")
	}
}

// TestPostTimeSensitivePattern_duplicateInDBReturns409 verifies that POSTing
// a rule ID that already exists in the DB overrides blob returns 409.
func TestPostTimeSensitivePattern_duplicateInDBReturns409(t *testing.T) {
	ts := &stubTSStore{
		blob: configstore.TimeSensitiveOverridesBlob{
			Rules: []configstore.TimeSensitiveOverrideRule{
				{ID: "existing-rule", Keywords: []string{"foo"}, Enabled: true},
			},
		},
	}
	h := newHandlerWithTSStore(ts)

	body := `{"id":"existing-rule","keywords":["bar"],"requireQuestionMark":false,"requireEntity":false,"languages":["en"],"enabled":true}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	if err := h.PostTimeSensitivePattern(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// Persistence: DELETE removes admin rule from DB blob

// TestDeleteTimeSensitivePattern_persistsRemoval verifies DELETE removes the
// rule from the DB blob.
func TestDeleteTimeSensitivePattern_persistsRemoval(t *testing.T) {
	ts := &stubTSStore{
		blob: configstore.TimeSensitiveOverridesBlob{
			Rules: []configstore.TimeSensitiveOverrideRule{
				{ID: "my-admin-rule", Keywords: []string{"foo"}, Enabled: true},
			},
		},
	}
	h := newHandlerWithTSStore(ts)

	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")
	c.SetParamNames("id")
	c.SetParamValues("my-admin-rule")

	if err := h.DeleteTimeSensitivePattern(c); err != nil {
		t.Fatalf("DeleteTimeSensitivePattern: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.saveCalls != 1 {
		t.Errorf("expected 1 SaveOverrides call, got %d", ts.saveCalls)
	}
	for _, r := range ts.blob.Rules {
		if r.ID == "my-admin-rule" {
			t.Error("my-admin-rule should have been removed from DB blob")
		}
	}
}

// TestDeleteTimeSensitivePattern_notInDBReturns404 verifies that deleting a
// non-seed ID that is not in the DB blob returns 404 (no longer returns "server
// restart" message — the rule simply was never added).
func TestDeleteTimeSensitivePattern_notInDBReturns404(t *testing.T) {
	ts := &stubTSStore{} // empty blob
	h := newHandlerWithTSStore(ts)

	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")
	c.SetParamNames("id")
	c.SetParamValues("ghost-rule")

	if err := h.DeleteTimeSensitivePattern(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// upsertOverrideRule: replacement branch

// TestUpsertOverrideRule_replacesExisting covers the branch where the rule ID
// already exists in the blob — it must be replaced in-place, not appended.
func TestUpsertOverrideRule_replacesExisting(t *testing.T) {
	blob := configstore.TimeSensitiveOverridesBlob{
		Rules: []configstore.TimeSensitiveOverrideRule{
			{ID: "r1", Keywords: []string{"old"}, Enabled: true},
			{ID: "r2", Keywords: []string{"keep"}, Enabled: true},
		},
	}
	updated := upsertOverrideRule(blob, configstore.TimeSensitiveOverrideRule{ID: "r1", Keywords: []string{"new"}, Enabled: false})
	if len(updated.Rules) != 2 {
		t.Fatalf("expected 2 rules after replace, got %d", len(updated.Rules))
	}
	for _, r := range updated.Rules {
		if r.ID == "r1" {
			if r.Keywords[0] != "new" || r.Enabled {
				t.Errorf("r1 not replaced: %+v", r)
			}
		}
	}
}

// removeOverrideRule: matching ID removed

// TestRemoveOverrideRule_removesMatchingID verifies that removeOverrideRule
// removes the rule matching the given ID and preserves others.
func TestRemoveOverrideRule_removesMatchingID(t *testing.T) {
	blob := configstore.TimeSensitiveOverridesBlob{
		Rules: []configstore.TimeSensitiveOverrideRule{
			{ID: "keep", Enabled: true},
			{ID: "gone", Enabled: true},
		},
	}
	result := removeOverrideRule(blob, "gone")
	if len(result.Rules) != 1 {
		t.Fatalf("expected 1 rule after remove, got %d", len(result.Rules))
	}
	if result.Rules[0].ID != "keep" {
		t.Errorf("wrong rule kept: %q", result.Rules[0].ID)
	}
}

// RegisterTimeSensitiveRoutes: mounts 5 routes

// TestRegisterTimeSensitiveRoutes_mountsFiveRoutes verifies that
// RegisterTimeSensitiveRoutes registers all 5 expected Echo routes.
func TestRegisterTimeSensitiveRoutes_mountsFiveRoutes(t *testing.T) {
	e := echo.New()
	h := newWithPool(nil, nil, nil, nil)
	g := e.Group("/api/admin")
	noopMW := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterTimeSensitiveRoutes(g, noopMW)

	routes := e.Routes()
	count := 0
	for _, r := range routes {
		if strings.Contains(r.Path, "time-sensitive") {
			count++
		}
	}
	if count != 5 {
		t.Errorf("expected 5 time-sensitive routes, got %d", count)
	}
}

// PutTimeSensitivePattern: loadOverrides error, saveOverrides error, hub error

// TestPutTimeSensitivePattern_loadOverridesError returns 500 when loadOverrides fails.
func TestPutTimeSensitivePattern_loadOverridesError(t *testing.T) {
	ts := &stubTSStore{getErr: errors.New("db: read error")}
	h := newHandlerWithTSStore(ts)

	body := `{"id":"weather","keywords":["rain"],"requireQuestionMark":false,"requireEntity":false,"languages":["en"],"enabled":false}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")
	c.SetParamNames("id")
	c.SetParamValues("weather")

	_ = h.PutTimeSensitivePattern(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

// TestPutTimeSensitivePattern_saveOverridesError returns 500 when saveOverrides fails.
func TestPutTimeSensitivePattern_saveOverridesError(t *testing.T) {
	ts := seededTSStore()
	ts.saveErr = errors.New("db: write error")
	h := newHandlerWithTSStore(ts)

	body := `{"id":"weather","keywords":["rain"],"requireQuestionMark":false,"requireEntity":false,"languages":["en"],"enabled":false}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")
	c.SetParamNames("id")
	c.SetParamValues("weather")

	_ = h.PutTimeSensitivePattern(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

// TestPutTimeSensitivePattern_hubError returns 502 (propagation_error) when the hub push fails.
func TestPutTimeSensitivePattern_hubError(t *testing.T) {
	ts := seededTSStore()
	fh := &fakeHub{err: errors.New("hub: unavailable")}
	h := newHandlerWithTSStore(ts)
	h.hub = fh

	body := `{"id":"weather","keywords":["rain"],"requireQuestionMark":false,"requireEntity":false,"languages":["en"],"enabled":false}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")
	c.SetParamNames("id")
	c.SetParamValues("weather")

	_ = h.PutTimeSensitivePattern(c)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 (F-0102 unified propagation policy), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "propagation_error") {
		t.Errorf("missing propagation_error envelope; body=%s", rec.Body.String())
	}
}

// PostTimeSensitivePattern: loadOverrides error, saveOverrides error, hub error

// TestPostTimeSensitivePattern_loadOverridesError returns 500 when loadOverrides fails.
func TestPostTimeSensitivePattern_loadOverridesError(t *testing.T) {
	ts := &stubTSStore{getErr: errors.New("db: read error")}
	h := newHandlerWithTSStore(ts)

	body := `{"id":"custom-new","keywords":["foo"],"requireQuestionMark":false,"requireEntity":false,"languages":["en"],"enabled":true}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	_ = h.PostTimeSensitivePattern(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

// TestPostTimeSensitivePattern_saveOverridesError returns 500 when saveOverrides fails.
func TestPostTimeSensitivePattern_saveOverridesError(t *testing.T) {
	ts := &stubTSStore{saveErr: errors.New("db: write error")}
	h := newHandlerWithTSStore(ts)

	body := `{"id":"brand-new","keywords":["unique"],"requireQuestionMark":false,"requireEntity":false,"languages":["en"],"enabled":true}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	_ = h.PostTimeSensitivePattern(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

// TestPostTimeSensitivePattern_hubError returns 502 (propagation_error) when the hub push fails.
func TestPostTimeSensitivePattern_hubError(t *testing.T) {
	ts := &stubTSStore{}
	fh := &fakeHub{err: errors.New("hub: unavailable")}
	h := newHandlerWithTSStore(ts)
	h.hub = fh

	body := `{"id":"new-unique-rule","keywords":["xyz"],"requireQuestionMark":false,"requireEntity":false,"languages":["en"],"enabled":true}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")

	_ = h.PostTimeSensitivePattern(c)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 (F-0102 unified propagation policy), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "propagation_error") {
		t.Errorf("missing propagation_error envelope; body=%s", rec.Body.String())
	}
}

// DeleteTimeSensitivePattern: saveOverrides error, hub error

// TestDeleteTimeSensitivePattern_saveOverridesError returns 500 when save fails.
func TestDeleteTimeSensitivePattern_saveOverridesError(t *testing.T) {
	ts := &stubTSStore{
		blob: configstore.TimeSensitiveOverridesBlob{
			Rules: []configstore.TimeSensitiveOverrideRule{
				{ID: "admin-rule", Keywords: []string{"test"}, Enabled: true},
			},
		},
		saveErr: errors.New("db: write error"),
	}
	h := newHandlerWithTSStore(ts)

	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")
	c.SetParamNames("id")
	c.SetParamValues("admin-rule")

	_ = h.DeleteTimeSensitivePattern(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

// TestDeleteTimeSensitivePattern_hubError returns 502 (propagation_error) when the hub push fails.
func TestDeleteTimeSensitivePattern_hubError(t *testing.T) {
	ts := &stubTSStore{
		blob: configstore.TimeSensitiveOverridesBlob{
			Rules: []configstore.TimeSensitiveOverrideRule{
				{ID: "admin-rule", Keywords: []string{"test"}, Enabled: true},
			},
		},
	}
	fh := &fakeHub{err: errors.New("hub: unavailable")}
	h := newHandlerWithTSStore(ts)
	h.hub = fh

	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "admin", "u-1")
	c.SetParamNames("id")
	c.SetParamValues("admin-rule")

	_ = h.DeleteTimeSensitivePattern(c)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 (F-0102 unified propagation policy), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "propagation_error") {
		t.Errorf("missing propagation_error envelope; body=%s", rec.Body.String())
	}
}

// New: nil pool (constructor guard)

// TestNew_NilPool verifies that New with a nil pool returns a non-nil Handler
// and does not panic. This covers the outer struct-init path of New.
func TestNew_NilPool(t *testing.T) {
	h := New(Deps{Pool: nil, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if h == nil {
		t.Fatal("New returned nil with nil Pool")
	}
}
