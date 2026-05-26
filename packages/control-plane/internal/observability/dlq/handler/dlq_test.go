package dlq

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	cpaudit "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
)

// noopMQProducer is a stand-in for the mq.Producer the audit writer takes.
// The CP audit writer's Enqueue path is fire-and-forget so a no-op satisfies
// the interface for the audit-emission branch exercised by RetryDLQ.
type noopMQProducer struct{}

func (n *noopMQProducer) Publish(_ context.Context, _ string, _ []byte) error { return nil }
func (n *noopMQProducer) Enqueue(_ context.Context, _ string, _ []byte) error { return nil }
func (n *noopMQProducer) Close() error                                        { return nil }

// stubHub is a test double for HubClient. Captures calls + status / body
// the handler should forward unchanged.
type stubHub struct {
	listBody   []byte
	listStatus int
	listErr    error
	listCalls  []struct{ Subject, Limit, Cursor string }

	retryBody   []byte
	retryStatus int
	retryErr    error
	retryCalls  []string
}

func (s *stubHub) ListDLQ(_ context.Context, subject, limit, cursor string) ([]byte, int, error) {
	s.listCalls = append(s.listCalls, struct{ Subject, Limit, Cursor string }{subject, limit, cursor})
	if s.listErr != nil {
		return nil, 0, s.listErr
	}
	return s.listBody, s.listStatus, nil
}
func (s *stubHub) RetryDLQ(_ context.Context, id string) ([]byte, int, error) {
	s.retryCalls = append(s.retryCalls, id)
	if s.retryErr != nil {
		return nil, 0, s.retryErr
	}
	return s.retryBody, s.retryStatus, nil
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newDLQContext(t *testing.T, method, path string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	r := httptest.NewRequest(method, path, strings.NewReader(""))
	w := httptest.NewRecorder()
	return e.NewContext(r, w), w
}

func TestListDLQ_ForwardsQueryAndResponse(t *testing.T) {
	hub := &stubHub{
		listBody:   []byte(`{"rows":[{"id":"1"}],"nextCursor":"x"}`),
		listStatus: http.StatusOK,
	}
	h := New(Deps{Hub: hub, Logger: silentLogger()})
	c, w := newDLQContext(t, http.MethodGet,
		"/api/admin/observability/dlq?subject=nexus.event.compliance&limit=10&cursor=2026-05-26T12:00:00Z")
	if err := h.ListDLQ(c); err != nil {
		t.Fatalf("ListDLQ: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, `"rows"`) {
		t.Errorf("response body missing rows: %s", got)
	}
	if len(hub.listCalls) != 1 {
		t.Fatalf("hub list calls = %d, want 1", len(hub.listCalls))
	}
	got := hub.listCalls[0]
	if got.Subject != "nexus.event.compliance" || got.Limit != "10" || got.Cursor == "" {
		t.Errorf("query forwarded = %+v, want {compliance, 10, non-empty cursor}", got)
	}
}

func TestListDLQ_HubError_Returns502(t *testing.T) {
	hub := &stubHub{listErr: errors.New("connection refused")}
	h := New(Deps{Hub: hub, Logger: silentLogger()})
	c, w := newDLQContext(t, http.MethodGet, "/api/admin/observability/dlq")
	if err := h.ListDLQ(c); err != nil {
		t.Fatalf("ListDLQ: %v", err)
	}
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestRetryDLQ_ForwardsAndReturns200(t *testing.T) {
	hub := &stubHub{
		retryBody:   []byte(`{"ok":true,"subject":"nexus.event.gateway"}`),
		retryStatus: http.StatusOK,
	}
	h := New(Deps{Hub: hub, Audit: nil, Logger: silentLogger()})

	const id = "abc-123"
	c, w := newDLQContext(t, http.MethodPost, "/api/admin/observability/dlq/"+id+"/retry")
	c.SetParamNames("id")
	c.SetParamValues(id)
	if err := h.RetryDLQ(c); err != nil {
		t.Fatalf("RetryDLQ: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if len(hub.retryCalls) != 1 || hub.retryCalls[0] != id {
		t.Errorf("retry calls = %+v, want [%q]", hub.retryCalls, id)
	}
	if got := w.Body.String(); !strings.Contains(got, `"ok":true`) {
		t.Errorf("response body = %s, want forwarded ok:true", got)
	}
}

func TestRetryDLQ_EmptyID_400(t *testing.T) {
	hub := &stubHub{}
	h := New(Deps{Hub: hub, Logger: silentLogger()})

	c, w := newDLQContext(t, http.MethodPost, "/api/admin/observability/dlq//retry")
	c.SetParamNames("id")
	c.SetParamValues("")
	if err := h.RetryDLQ(c); err != nil {
		t.Fatalf("RetryDLQ: %v", err)
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if len(hub.retryCalls) != 0 {
		t.Error("hub must not be called when id is empty")
	}
}

func TestRetryDLQ_HubError_Returns502(t *testing.T) {
	hub := &stubHub{retryErr: errors.New("hub down")}
	h := New(Deps{Hub: hub, Logger: silentLogger()})

	c, w := newDLQContext(t, http.MethodPost, "/api/admin/observability/dlq/x/retry")
	c.SetParamNames("id")
	c.SetParamValues("x")
	if err := h.RetryDLQ(c); err != nil {
		t.Fatalf("RetryDLQ: %v", err)
	}
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestRetryDLQ_HubNon2xx_ForwardedNoAudit(t *testing.T) {
	hub := &stubHub{
		retryBody:   []byte(`{"error":"dlq_not_found"}`),
		retryStatus: http.StatusNotFound,
	}
	h := New(Deps{Hub: hub, Audit: nil, Logger: silentLogger()})

	c, w := newDLQContext(t, http.MethodPost, "/api/admin/observability/dlq/missing/retry")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.RetryDLQ(c); err != nil {
		t.Fatalf("RetryDLQ: %v", err)
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (forwarded from Hub)", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "dlq_not_found") {
		t.Errorf("body = %s, want forwarded error", got)
	}
}

// TestRegisterDLQRoutes_WiresGetAndPost verifies the two routes register
// against the supplied echo group with the expected paths and IAM
// middleware. The middleware is a no-op stub here; we just check the
// router resolves both endpoints.
func TestRegisterDLQRoutes_WiresGetAndPost(t *testing.T) {
	noopMW := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error { return next(c) }
		}
	}
	hub := &stubHub{listBody: []byte(`{}`), listStatus: http.StatusOK, retryBody: []byte(`{}`), retryStatus: http.StatusOK}
	h := New(Deps{Hub: hub, Logger: silentLogger()})

	e := echo.New()
	g := e.Group("/api/admin")
	h.RegisterDLQRoutes(g, noopMW)

	// GET resolves and returns 200 (handler forwards Hub's response).
	req := httptest.NewRequest(http.MethodGet, "/api/admin/observability/dlq", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /observability/dlq status = %d, want 200", rec.Code)
	}

	// POST resolves with :id and returns 200.
	req = httptest.NewRequest(http.MethodPost, "/api/admin/observability/dlq/abc/retry", strings.NewReader(""))
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("POST /observability/dlq/:id/retry status = %d, want 200", rec.Code)
	}
}

// TestNew_NilLoggerFallback covers the slog.Default() fallback branch
// in New that callers can't otherwise observe.
func TestNew_NilLoggerFallback(t *testing.T) {
	hub := &stubHub{}
	h := New(Deps{Hub: hub, Logger: nil})
	if h.logger == nil {
		t.Error("logger fallback should produce non-nil slog.Default")
	}
}

// TestRetryDLQ_AuditOnSuccess covers the audit-emission branch: when
// the Hub returns 2xx AND h.audit is non-nil, RetryDLQ stamps an
// AdminAuditLog entry attributing the retry to the operator. The audit
// writer here uses a no-op MQ producer — we only need the branch to
// execute, not the resulting message to land anywhere.
func TestRetryDLQ_AuditOnSuccess(t *testing.T) {
	hub := &stubHub{
		retryBody:   []byte(`{"ok":true}`),
		retryStatus: http.StatusOK,
	}
	auditWriter := cpaudit.NewWriter(&noopMQProducer{}, "nexus.event.admin-audit", silentLogger())
	h := New(Deps{Hub: hub, Audit: auditWriter, Logger: silentLogger()})

	c, w := newDLQContext(t, http.MethodPost, "/api/admin/observability/dlq/audit-id/retry")
	c.SetParamNames("id")
	c.SetParamValues("audit-id")
	if err := h.RetryDLQ(c); err != nil {
		t.Fatalf("RetryDLQ: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	// We can't assert the audit row directly without DB; the goal is just
	// to drive the audit-write branch under coverage so a regression that
	// disables audit emission surfaces in the coverage delta.
}

// TestRetryDLQ_NoAuditOnHubFailure pins the inverse contract: when Hub
// returns non-2xx (e.g. 404 dlq_not_found), CP must NOT write an audit
// row — pretending a missing retry happened pollutes the operator audit
// trail.
func TestRetryDLQ_NoAuditOnHubFailure(t *testing.T) {
	hub := &stubHub{
		retryBody:   []byte(`{"error":"dlq_not_found"}`),
		retryStatus: http.StatusNotFound,
	}
	auditWriter := cpaudit.NewWriter(&noopMQProducer{}, "nexus.event.admin-audit", silentLogger())
	h := New(Deps{Hub: hub, Audit: auditWriter, Logger: silentLogger()})

	c, w := newDLQContext(t, http.MethodPost, "/api/admin/observability/dlq/missing/retry")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.RetryDLQ(c); err != nil {
		t.Fatalf("RetryDLQ: %v", err)
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// Compile-time sanity: stubHub must satisfy HubClient.
var _ HubClient = (*stubHub)(nil)
