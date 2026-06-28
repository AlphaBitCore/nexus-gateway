package virtualkey

import (
	"bytes"
	"context"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/virtualkeys/vkstore"
	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// Test helpers ported from the routing-domain precedent
// (handler/routing/test_helpers_test.go + handler_db_test.go) per
// R6 runbook §4.2 option 1 — local copies until a handler/util/
// subpackage exists.

// hubSpy captures Hub calls made by virtualkey handlers during unit tests.
// virtualkey handlers need NotifyConfigChange (for VK CRUD invalidate-by-
// hash) and InvalidateConfigE (for approve/renew/revoke fail-loud),
// matching the HubVKInvalidator interface in handler.go. invalidateErr lets a
// test drive the push-failure → HTTP 502 branch.
type hubSpy struct {
	mu              sync.Mutex
	invalidateCalls []hubInvalidateCall
	invalidateErr   error
	notifyCalls     []hub.ConfigChangeRequest
	notifyErr       error
	notifyResponse  *hub.ConfigChangeResponse
}

type hubInvalidateCall struct {
	ThingType string
	ConfigKey string
}

func (s *hubSpy) InvalidateConfigE(_ context.Context, thingType, configKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidateCalls = append(s.invalidateCalls, hubInvalidateCall{ThingType: thingType, ConfigKey: configKey})
	return s.invalidateErr
}

func (s *hubSpy) NotifyConfigChange(_ context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifyCalls = append(s.notifyCalls, req)
	if s.notifyErr != nil {
		return nil, s.notifyErr
	}
	if s.notifyResponse != nil {
		return s.notifyResponse, nil
	}
	return &hub.ConfigChangeResponse{OK: true, Version: 1, ThingsNotified: 1, ThingsOnline: 1}, nil
}

func (s *hubSpy) NotifyCalls() []hub.ConfigChangeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]hub.ConfigChangeRequest, len(s.notifyCalls))
	copy(out, s.notifyCalls)
	return out
}

// auditSpy captures MQ enqueues so tests can assert audit entries.
type auditSpy struct {
	mu    sync.Mutex
	calls [][]byte
}

func (a *auditSpy) Publish(context.Context, string, []byte) error { return nil }
func (a *auditSpy) Enqueue(_ context.Context, _ string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	copied := make([]byte, len(data))
	copy(copied, data)
	a.calls = append(a.calls, copied)
	return nil
}
func (a *auditSpy) Close() error { return nil }

func (a *auditSpy) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func (a *auditSpy) last() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(a.calls[len(a.calls)-1], &m)
	return m
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testLogWriter routes slog output into t.Log so error-branch handler
// log calls surface in -v test runs without spamming stdout.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// newHandlerWithMockDB wires a virtualkey.Handler with a pgxmock pool, a Hub
// spy, an audit spy and a t.Log-routed logger.
func newHandlerWithMockDB(t *testing.T) (*Handler, pgxmock.PgxPoolIface, *hubSpy, *auditSpy) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	hub := &hubSpy{}
	aud := &auditSpy{}
	logger := slog.New(slog.NewTextHandler(testLogWriter{t: t}, nil))
	h := New(Deps{
		Pool:   mock,
		Hub:    hub,
		Audit:  audit.NewWriter(aud, "admin-audit", silentLogger()),
		Logger: logger,
	})
	return h, mock, hub, aud
}

// echoContext builds an Echo context with an AdminAuth attached so handlers
// that call audit.EntryFor (or actorFromContext) do not crash.
func echoContext(req *http.Request, rec *httptest.ResponseRecorder, userName, userID string) echo.Context {
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{
		KeyID:             userID,
		KeyName:           userName,
		AuthPrincipalType: "admin_user",
	})
	return c
}

// makeJSONReq builds an Echo context wired with a JSON body + admin auth.
func makeJSONReq(t *testing.T, method, target, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, bytes.NewReader([]byte(body)))
		r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	rec := httptest.NewRecorder()
	c := echoContext(r, rec, "Admin", "admin-1")
	return c, rec
}

// assertErrorEnvelope verifies the standard {"error":{...}} envelope shape.
// Passing wantCode/wantType as "" skips that field's check.
func assertErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantCode, wantType string) {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error body: %v; raw=%s", err, rec.Body.String())
	}
	if wantCode != "" && body.Error.Code != wantCode {
		t.Errorf("error.code = %q want %q", body.Error.Code, wantCode)
	}
	if wantType != "" && body.Error.Type != wantType {
		t.Errorf("error.type = %q want %q", body.Error.Type, wantType)
	}
}

// vkCols mirrors the SELECT in store/virtual_key.go::vkColumns.
var vkCols = []string{
	"id", "name", "keyHash", "keyPrefix", "projectId", "sourceApp", "enabled",
	"expiresAt", "rateLimitRpm", "compareEndpointRateLimitRpm",
	"allowedModels", "ownerId", "createdBy", "createdAt", "updatedAt",
	"vkType", "vkStatus", "approvedBy", "approvedAt", "rejectedBy", "rejectedAt", "rejectReason",
}

// makeVKRow returns one row matching the production scanner.
// ownerID is passed by value so callers can pin the owner-mismatch branch
// (Personal/Admin self-service ownership check). The row is an "application"
// VK — the type the admin surface operates on.
func makeVKRow(id, name string, ownerID *string) []any {
	return makeVKRowTyped(id, name, ownerID, "application")
}

// makeVKRowTyped is makeVKRow with an explicit vkType, used by tests that need
// to pin the cap-scoping boundary (application is capped, personal is exempt).
func makeVKRowTyped(id, name string, ownerID *string, vkType string) []any {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expires := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	hash := "hash-" + id
	prefix := "nvk_abcd"
	vkStatus := "active"
	return []any{
		id, name, &hash, &prefix,
		nil, // projectId
		nil, // sourceApp
		true,
		&expires,
		nil, nil, // rateLimitRpm / compareEndpoint
		json.RawMessage(`["m1"]`), // allowedModels
		ownerID,
		nil,      // createdBy
		now, now, // createdAt/updatedAt
		&vkType, &vkStatus,
		nil, nil, nil, nil, nil, // approvedBy/approvedAt/rejectedBy/rejectedAt/rejectReason
	}
}

// anyN returns N pgxmock.AnyArg() entries so ExpectQuery.WithArgs can match
// SQL statements with many positional parameters without listing them all.
func anyN(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = pgxmock.AnyArg()
	}
	return out
}

// failingReader implements io.Reader by always returning an error, used to
// drive the io.ReadAll failure branch in UpdateVirtualKey.
type failingReader struct{}

func (failingReader) Read(_ []byte) (int, error) { return 0, errReader }

// errReader is a sentinel returned by failingReader.Read. Kept package-level
// so the failingReader stays a zero-value type.
var errReader = errReadFailed

type errReadFailedT struct{}

func (errReadFailedT) Error() string { return "simulated read error" }

var errReadFailed = errReadFailedT{}

// strPtr returns &s for tabular tests building expected pointer fields.
func strPtr(s string) *string { return &s }

// iamMWNoop returns an IAM middleware that always permits the request.
// Used by RegisterRoutes assertions where we only care about path/verb
// mounts, not the IAM check itself.
func iamMWNoop(_ string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
}

// poolFromMock returns a vkstore.PgxPool from the supplied pgxmock pool.
// Used by nil-Hub test variants that bypass newHandlerWithMockDB.
func poolFromMock(mock pgxmock.PgxPoolIface) vkstore.PgxPool {
	return mock
}
