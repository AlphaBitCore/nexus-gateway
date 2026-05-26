package bootstrap

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"
)

// newEchoCtx creates a minimal Echo context bound to a ResponseRecorder.
func newEchoCtx(t *testing.T) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/public/agent-bootstrap", nil)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

// decodeBootstrap parses the recorder body as bootstrapResponse.
func decodeBootstrap(t *testing.T, rec *httptest.ResponseRecorder) bootstrapResponse {
	t.Helper()
	var resp bootstrapResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal bootstrap response: %v (body=%s)", err, rec.Body.String())
	}
	return resp
}

// TestNormaliseAgentBootstrapMode covers the mode-normalisation function.
func TestNormaliseAgentBootstrapMode(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"local-login", "enterprise-login"},
		{"enterprise-login", "enterprise-login"},
		{"mtls-only", "mtls-only"},
		{"", ""},
		{"custom-mode", "custom-mode"},
	}
	for _, tc := range cases {
		got := normaliseAgentBootstrapMode(tc.raw)
		if got != tc.want {
			t.Errorf("normaliseAgentBootstrapMode(%q) = %q; want %q", tc.raw, got, tc.want)
		}
	}
}

// TestAgentBootstrapHandler_NilDB verifies that a nil DB returns the safe
// default ("mtls-only") and the configured CpURL without touching any
// database.
func TestAgentBootstrapHandler_NilDB(t *testing.T) {
	h := &AgentBootstrapHandler{CpURL: "https://cp.example.com"}
	c, rec := newEchoCtx(t)
	if err := h.Handle(c); err != nil {
		t.Fatalf("Handle with nil DB: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d; want 200", rec.Code)
	}
	resp := decodeBootstrap(t, rec)
	if resp.ControlPlaneURL != "https://cp.example.com" {
		t.Errorf("controlPlaneURL = %q; want https://cp.example.com", resp.ControlPlaneURL)
	}
	if resp.DeviceAuthMode != "mtls-only" {
		t.Errorf("deviceAuthMode = %q; want mtls-only", resp.DeviceAuthMode)
	}
}

// TestAgentBootstrapHandler_DBReturnsMode verifies that a device.auth.mode
// row in system_metadata overrides the default and is delivered to callers.
func TestAgentBootstrapHandler_DBReturnsMode(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	modeJSON, _ := json.Marshal("enterprise-login")
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("device.auth.mode").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(modeJSON))

	h := &AgentBootstrapHandler{CpURL: "https://cp.example.com", DB: mock}
	c, rec := newEchoCtx(t)
	if err := h.Handle(c); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d; want 200", rec.Code)
	}
	resp := decodeBootstrap(t, rec)
	if resp.DeviceAuthMode != "enterprise-login" {
		t.Errorf("deviceAuthMode = %q; want enterprise-login", resp.DeviceAuthMode)
	}
}

// TestAgentBootstrapHandler_LocalLoginNormalised verifies that the
// "local-login" raw value is collapsed to "enterprise-login" in the response.
func TestAgentBootstrapHandler_LocalLoginNormalised(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	modeJSON, _ := json.Marshal("local-login")
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("device.auth.mode").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(modeJSON))

	h := &AgentBootstrapHandler{CpURL: "https://cp.example.com", DB: mock}
	c, rec := newEchoCtx(t)
	if err := h.Handle(c); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	resp := decodeBootstrap(t, rec)
	if resp.DeviceAuthMode != "enterprise-login" {
		t.Errorf("deviceAuthMode = %q; want enterprise-login (local-login normalised)", resp.DeviceAuthMode)
	}
}

// TestAgentBootstrapHandler_DBNoRows verifies that when system_metadata has
// no row for the key the handler falls back to "mtls-only" without error.
func TestAgentBootstrapHandler_DBNoRows(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("device.auth.mode").
		WillReturnError(pgx.ErrNoRows)

	h := &AgentBootstrapHandler{CpURL: "https://cp.example.com", DB: mock}
	c, rec := newEchoCtx(t)
	if err := h.Handle(c); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d; want 200", rec.Code)
	}
	resp := decodeBootstrap(t, rec)
	if resp.DeviceAuthMode != "mtls-only" {
		t.Errorf("deviceAuthMode = %q; want mtls-only (ErrNoRows fallback)", resp.DeviceAuthMode)
	}
}

// TestAgentBootstrapHandler_DBError verifies that a real DB error propagates
// as a 500 response (not a panic).
func TestAgentBootstrapHandler_DBError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	dbErr := errors.New("connection refused")
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("device.auth.mode").
		WillReturnError(dbErr)

	h := &AgentBootstrapHandler{CpURL: "https://cp.example.com", DB: mock}
	c, rec := newEchoCtx(t)
	if err := h.Handle(c); err != nil {
		t.Fatalf("Handle returned error; want inline JSON: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d; want 500 on DB error", rec.Code)
	}
}

// TestAgentBootstrapHandler_CacheHit verifies the 60-second in-memory cache:
// a second call within TTL must NOT query the DB again.
func TestAgentBootstrapHandler_CacheHit(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	modeJSON, _ := json.Marshal("mtls-only")
	// Only one DB query expected — the second request must hit the cache.
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("device.auth.mode").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(modeJSON))

	h := &AgentBootstrapHandler{CpURL: "https://cp.example.com", DB: mock}

	// First call populates cache.
	c1, _ := newEchoCtx(t)
	if err := h.Handle(c1); err != nil {
		t.Fatalf("Handle (1st): %v", err)
	}

	// Second call within TTL must reuse the cache.
	c2, rec2 := newEchoCtx(t)
	if err := h.Handle(c2); err != nil {
		t.Fatalf("Handle (2nd): %v", err)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("status %d; want 200 on cache hit", rec2.Code)
	}

	// pgxmock will fail the test if any unexpected query is executed.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB queries on cache hit: %v", err)
	}
}

// TestAgentBootstrapHandler_CacheExpires verifies that after the TTL expires
// the next request fetches from DB again.
func TestAgentBootstrapHandler_CacheExpires(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	modeJSON, _ := json.Marshal("mtls-only")
	// Two DB queries expected — one per expired cache.
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("device.auth.mode").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(modeJSON))
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("device.auth.mode").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(modeJSON))

	h := &AgentBootstrapHandler{CpURL: "https://cp.example.com", DB: mock}

	// First call populates cache then backdates the cache entry so it is expired.
	c1, _ := newEchoCtx(t)
	if err := h.Handle(c1); err != nil {
		t.Fatalf("Handle (1st): %v", err)
	}
	// Backdating: replace the stored entry with one whose fetched time is 2 minutes ago.
	old := h.cache.Load()
	if old != nil {
		h.cache.Store(&bootstrapCacheEntry{body: old.body, fetched: time.Now().Add(-2 * time.Minute)})
	}

	// Second call should go to DB because the cache is expired.
	c2, rec2 := newEchoCtx(t)
	if err := h.Handle(c2); err != nil {
		t.Fatalf("Handle (2nd): %v", err)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("status %d; want 200 after cache expiry", rec2.Code)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB not queried after cache expiry: %v", err)
	}
}

// TestJtiCache_MarkSeen verifies the replay guard invariants:
// empty JTI rejected, first-seen accepted, replay rejected.
func TestJtiCache_MarkSeen(t *testing.T) {
	c := &jtiCache{
		entries: make(map[string]time.Time),
		stopCh:  make(chan struct{}),
		now:     time.Now,
	}
	exp := time.Now().Add(5 * time.Minute)

	// Empty JTI must be rejected.
	if c.MarkSeen("", exp) {
		t.Error("empty jti must return false")
	}
	// First seen: must accept.
	if !c.MarkSeen("jti-abc", exp) {
		t.Error("first MarkSeen must return true")
	}
	// Replay: same JTI must be rejected.
	if c.MarkSeen("jti-abc", exp) {
		t.Error("second MarkSeen (replay) must return false")
	}
}

// TestJtiCache_Sweep verifies that expired JTI entries are removed by sweep.
func TestJtiCache_Sweep(t *testing.T) {
	now := time.Now()
	c := &jtiCache{
		entries: make(map[string]time.Time),
		stopCh:  make(chan struct{}),
		now:     func() time.Time { return now },
	}
	// Insert one expired and one valid entry.
	c.entries["expired"] = now.Add(-1 * time.Second)
	c.entries["valid"] = now.Add(5 * time.Minute)

	c.sweep()

	if _, exists := c.entries["expired"]; exists {
		t.Error("sweep must remove expired entry")
	}
	if _, exists := c.entries["valid"]; !exists {
		t.Error("sweep must keep non-expired entry")
	}
}

// TestJtiCache_StopIdempotent verifies that calling Stop multiple times does
// not panic.
func TestJtiCache_StopIdempotent(t *testing.T) {
	c := newJTICache()
	c.Stop()
	c.Stop() // must not panic
}
