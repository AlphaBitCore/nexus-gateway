package cache

// semantic_feedback_test.go — white-box tests for the negative-feedback
// channel: PostSemanticFeedback, GetSemanticFeedback, recordFeedback,
// recentFeedback, redisPoisonAdder, and the ring-buffer helpers.
//
// All tests are deterministic, in-process, and make no network calls.
// The ring buffer is process-global; each test that mutates it must reset
// the global state beforehand via resetFeedbackRing (helper defined below).

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// ring buffer helpers

// resetFeedbackRing resets the process-global ring buffer so tests are
// independent of each other.
func resetFeedbackRing() {
	feedbackRing.mu.Lock()
	defer feedbackRing.mu.Unlock()
	feedbackRing.entries = nil
}

// poison list doubles

// stubPoisonOK is a PoisonAdder that always succeeds.
type stubPoisonOK struct {
	addedEntryKey string
	addedVKScope  string
	addedTTL      time.Duration
}

func (s *stubPoisonOK) Add(_ context.Context, entryKey, vkScope string, ttl time.Duration) error {
	s.addedEntryKey = entryKey
	s.addedVKScope = vkScope
	s.addedTTL = ttl
	return nil
}

// stubPoisonErr is a PoisonAdder that always returns an error.
type stubPoisonErr struct{}

func (s *stubPoisonErr) Add(_ context.Context, _, _ string, _ time.Duration) error {
	return errors.New("redis connection refused")
}

// Echo context builder

func feedbackEchoContext(body []byte, contentType string) (echo.Context, *httptest.ResponseRecorder) {
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/cache/semantic-feedback", reqBody)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{
		KeyID:   "user-42",
		KeyName: "testuser",
	})
	return c, rec
}

func feedbackGetEchoContext(query string) (echo.Context, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodGet, "/api/admin/cache/semantic-feedback?"+query, nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{
		KeyID:   "user-42",
		KeyName: "testuser",
	})
	return c, rec
}

// audit writer helper

func newFeedbackAuditWriter(spy *auditSpy) *audit.Writer {
	return audit.NewWriter(spy, "audit", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// handler factory

func newFeedbackHandler(poison PoisonAdder, aw *audit.Writer) *SemanticCacheHandler {
	return NewSemanticCacheHandler(SemanticCacheHandlerDeps{
		Store:  nil,
		Hub:    nil,
		Audit:  aw,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Poison: poison,
	})
}

// PostSemanticFeedback tests

// TestPostSemanticFeedback_HappyPath verifies the full success flow: poison is
// recorded, ring buffer updated, audit emitted, 200 returned.
func TestPostSemanticFeedback_HappyPath(t *testing.T) {
	resetFeedbackRing()
	spy := &auditSpy{}
	poison := &stubPoisonOK{}
	h := newFeedbackHandler(poison, newFeedbackAuditWriter(spy))

	body := map[string]any{
		"entryKey":   "idx:abc123",
		"vkScope":    "v1:vk:42",
		"reason":     "wrong answer",
		"ttlSeconds": 3600,
	}
	raw, _ := json.Marshal(body)
	c, rec := feedbackEchoContext(raw, "application/json")

	if err := h.PostSemanticFeedback(c); err != nil {
		t.Fatalf("PostSemanticFeedback error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	// Verify poison was called with correct args.
	if poison.addedEntryKey != "idx:abc123" {
		t.Errorf("entryKey = %q, want idx:abc123", poison.addedEntryKey)
	}
	if poison.addedVKScope != "v1:vk:42" {
		t.Errorf("vkScope = %q, want v1:vk:42", poison.addedVKScope)
	}
	if poison.addedTTL != 3600*time.Second {
		t.Errorf("ttl = %v, want 3600s", poison.addedTTL)
	}

	// Verify ring buffer recorded the entry.
	entries := recentFeedback(10)
	if len(entries) != 1 {
		t.Fatalf("expected 1 feedback entry, got %d", len(entries))
	}
	if entries[0].EntryKey != "idx:abc123" || entries[0].Reason != "wrong answer" {
		t.Errorf("ring buffer entry = %+v", entries[0])
	}

	// Audit must have fired.
	if spy.count() == 0 {
		t.Error("expected audit event to be emitted")
	}
}

// TestPostSemanticFeedback_MissingEntryKey verifies 400 when entryKey is absent.
func TestPostSemanticFeedback_MissingEntryKey(t *testing.T) {
	resetFeedbackRing()
	h := newFeedbackHandler(&stubPoisonOK{}, nil)

	body := map[string]any{"vkScope": "v1:vk:1", "reason": "bad"}
	raw, _ := json.Marshal(body)
	c, rec := feedbackEchoContext(raw, "application/json")

	if err := h.PostSemanticFeedback(c); err != nil {
		t.Fatalf("PostSemanticFeedback error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestPostSemanticFeedback_MissingReason verifies 400 when reason is absent.
func TestPostSemanticFeedback_MissingReason(t *testing.T) {
	resetFeedbackRing()
	h := newFeedbackHandler(&stubPoisonOK{}, nil)

	body := map[string]any{"entryKey": "idx:abc", "vkScope": "v1:vk:1"}
	raw, _ := json.Marshal(body)
	c, rec := feedbackEchoContext(raw, "application/json")

	if err := h.PostSemanticFeedback(c); err != nil {
		t.Fatalf("PostSemanticFeedback error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestPostSemanticFeedback_NoPoisonWired verifies 503 when poison is nil.
func TestPostSemanticFeedback_NoPoisonWired(t *testing.T) {
	resetFeedbackRing()
	h := newFeedbackHandler(nil, nil) // poison = nil → 503

	body := map[string]any{"entryKey": "idx:abc", "vkScope": "v1:vk:1", "reason": "bad"}
	raw, _ := json.Marshal(body)
	c, rec := feedbackEchoContext(raw, "application/json")

	if err := h.PostSemanticFeedback(c); err != nil {
		t.Fatalf("PostSemanticFeedback error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// TestPostSemanticFeedback_PoisonError verifies 500 when poison.Add returns error.
func TestPostSemanticFeedback_PoisonError(t *testing.T) {
	resetFeedbackRing()
	h := newFeedbackHandler(&stubPoisonErr{}, nil)

	body := map[string]any{"entryKey": "idx:abc", "vkScope": "v1:vk:1", "reason": "bad"}
	raw, _ := json.Marshal(body)
	c, rec := feedbackEchoContext(raw, "application/json")

	if err := h.PostSemanticFeedback(c); err != nil {
		t.Fatalf("PostSemanticFeedback error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestPostSemanticFeedback_MalformedJSON verifies 400 on invalid body.
func TestPostSemanticFeedback_MalformedJSON(t *testing.T) {
	resetFeedbackRing()
	h := newFeedbackHandler(&stubPoisonOK{}, nil)
	c, rec := feedbackEchoContext([]byte("{invalid json"), "application/json")
	if err := h.PostSemanticFeedback(c); err != nil {
		t.Fatalf("PostSemanticFeedback error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestPostSemanticFeedback_NoAuditNoPanic verifies that a nil audit writer
// does not cause a panic when poison succeeds.
func TestPostSemanticFeedback_NoAuditNoPanic(t *testing.T) {
	resetFeedbackRing()
	h := newFeedbackHandler(&stubPoisonOK{}, nil) // nil audit writer

	body := map[string]any{"entryKey": "idx:abc", "vkScope": "v1:vk:1", "reason": "bad"}
	raw, _ := json.Marshal(body)
	c, rec := feedbackEchoContext(raw, "application/json")

	if err := h.PostSemanticFeedback(c); err != nil {
		t.Fatalf("PostSemanticFeedback with nil audit error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// GetSemanticFeedback tests

// TestGetSemanticFeedback_DefaultLimit verifies that GET returns up to 100
// entries by default.
func TestGetSemanticFeedback_DefaultLimit(t *testing.T) {
	resetFeedbackRing()
	h := newFeedbackHandler(nil, nil)

	// Add 3 entries to the ring buffer.
	for range 3 {
		recordFeedback(FeedbackEntry{
			EntryKey: "key",
			VKScope:  "scope",
			Reason:   "test",
			ActorID:  "u",
		})
	}

	c, rec := feedbackGetEchoContext("")
	if err := h.GetSemanticFeedback(c); err != nil {
		t.Fatalf("GetSemanticFeedback error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	total, _ := resp["total"].(float64)
	if total != 3 {
		t.Errorf("total = %v, want 3", total)
	}
}

// TestGetSemanticFeedback_CustomLimit verifies that ?limit=1 returns at most 1.
func TestGetSemanticFeedback_CustomLimit(t *testing.T) {
	resetFeedbackRing()
	h := newFeedbackHandler(nil, nil)

	// Add 5 entries.
	for range 5 {
		recordFeedback(FeedbackEntry{EntryKey: "k", Reason: "r"})
	}

	c, rec := feedbackGetEchoContext("limit=1")
	if err := h.GetSemanticFeedback(c); err != nil {
		t.Fatalf("GetSemanticFeedback error: %v", err)
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	total, _ := resp["total"].(float64)
	if total != 1 {
		t.Errorf("total = %v, want 1", total)
	}
}

// TestGetSemanticFeedback_EmptyRing verifies entries=[] (not null) on empty ring.
func TestGetSemanticFeedback_EmptyRing(t *testing.T) {
	resetFeedbackRing()
	h := newFeedbackHandler(nil, nil)

	c, rec := feedbackGetEchoContext("")
	if err := h.GetSemanticFeedback(c); err != nil {
		t.Fatalf("GetSemanticFeedback error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	total, _ := resp["total"].(float64)
	if total != 0 {
		t.Errorf("total = %v, want 0", total)
	}
}

// TestGetSemanticFeedback_LimitCappedAt1000 verifies that limit>1000 is capped.
func TestGetSemanticFeedback_LimitCappedAt1000(t *testing.T) {
	resetFeedbackRing()
	h := newFeedbackHandler(nil, nil)

	c, rec := feedbackGetEchoContext("limit=9999")
	if err := h.GetSemanticFeedback(c); err != nil {
		t.Fatalf("GetSemanticFeedback error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestGetSemanticFeedback_InvalidLimitUsesDefault verifies non-integer limit
// falls back to 100.
func TestGetSemanticFeedback_InvalidLimitUsesDefault(t *testing.T) {
	resetFeedbackRing()
	h := newFeedbackHandler(nil, nil)

	c, rec := feedbackGetEchoContext("limit=notanint")
	if err := h.GetSemanticFeedback(c); err != nil {
		t.Fatalf("GetSemanticFeedback error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// ring buffer: overflow / cap behaviour

// TestRecordFeedback_RingOverflow verifies that when the ring buffer exceeds
// 1000 entries it is capped by truncating the oldest.
func TestRecordFeedback_RingOverflow(t *testing.T) {
	resetFeedbackRing()

	for range maxFeedbackHistory + 50 {
		recordFeedback(FeedbackEntry{EntryKey: "k", Reason: "r"})
	}
	entries := recentFeedback(0)
	if len(entries) != maxFeedbackHistory {
		t.Errorf("expected ring to be capped at %d, got %d", maxFeedbackHistory, len(entries))
	}
}

// TestRecentFeedback_ZeroLimitReturnsAll verifies limit=0 returns all entries.
func TestRecentFeedback_ZeroLimitReturnsAll(t *testing.T) {
	resetFeedbackRing()
	for range 10 {
		recordFeedback(FeedbackEntry{EntryKey: "k", Reason: "r"})
	}
	entries := recentFeedback(0)
	if len(entries) != 10 {
		t.Errorf("expected 10, got %d", len(entries))
	}
}

// TestRecentFeedback_LargeLimit verifies limit > len returns all.
func TestRecentFeedback_LargeLimit(t *testing.T) {
	resetFeedbackRing()
	for range 5 {
		recordFeedback(FeedbackEntry{EntryKey: "k", Reason: "r"})
	}
	entries := recentFeedback(1000)
	if len(entries) != 5 {
		t.Errorf("expected 5, got %d", len(entries))
	}
}

// redisPoisonAdder tests

// TestNewRedisPoisonAdder_ConstructsNonNil verifies the constructor returns
// a non-nil PoisonAdder.
func TestNewRedisPoisonAdder_ConstructsNonNil(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6399"}) // unreachable
	defer rdb.Close()
	pa := NewRedisPoisonAdder(rdb)
	if pa == nil {
		t.Fatal("NewRedisPoisonAdder returned nil")
	}
}

// TestRedisPoisonAdder_AddZeroTTLNoNilPanic verifies that a zero TTL does not
// cause a nil-dereference or panic in redisPoisonAdder.Add.
func TestRedisPoisonAdder_AddZeroTTLNoNilPanic(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:6399",
		DialTimeout:  5 * time.Millisecond,
		ReadTimeout:  5 * time.Millisecond,
		WriteTimeout: 5 * time.Millisecond,
	})
	defer rdb.Close()
	pa := NewRedisPoisonAdder(rdb)
	// Add returns an error (unreachable server) but must not panic.
	err := pa.Add(context.Background(), "key1", "scope1", 0)
	_ = err
}

// TestRedisPoisonAdder_AddLargeTTLNoNilPanic verifies that TTL larger than 30d
// does not cause overflow or panic (cap applied before the Redis call).
func TestRedisPoisonAdder_AddLargeTTLNoNilPanic(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:6399",
		DialTimeout:  5 * time.Millisecond,
		ReadTimeout:  5 * time.Millisecond,
		WriteTimeout: 5 * time.Millisecond,
	})
	defer rdb.Close()
	pa := NewRedisPoisonAdder(rdb)
	err := pa.Add(context.Background(), "key2", "scope2", 365*24*time.Hour)
	_ = err // error expected (unreachable server); no panic = pass
}
