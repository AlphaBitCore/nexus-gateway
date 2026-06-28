// Audit-emit verification for passthrough handler write paths.
//
// Locks the contract that PutGlobal / PutAdapter / PutProvider /
// DeleteAdapter / DeleteProvider each publish exactly one
// admin-audit MQ message under the expected (entityType, action,
// entityId) shape after the upstream mutation commits. Without
// these tests an absent audit.Writer wiring regresses silently.
package passthrough

import (
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// noRowsErr returns pgx.ErrNoRows so readTierState short-circuits to
// (nil, nil) — the "no prior row" branch.
func noRowsErr() error { return pgx.ErrNoRows }

// echoCtxWithAdminAuth builds an Echo context with the AdminAuth identity
// attached — both actor() and audit.EntryFor read it via
// middleware.AdminAuthFromContext.
func echoCtxWithAdminAuth(method, path, body string) (echo.Context, *httptest.ResponseRecorder) {
	c, rec := echoCtxWithUser(method, path, body)
	middleware.WithAdminAuth(c, &auth.AdminAuth{
		KeyID:             "admin-1",
		KeyName:           "Alice",
		AuthPrincipalType: "admin_user",
	})
	return c, rec
}

// passthroughAuditSpy captures every Enqueue payload so individual
// tests can decode and assert one or more audit envelopes. Mirrors
// the auditSpy in packages/control-plane/internal/handler — duplicated
// here to keep the per-handler test package self-contained.
type passthroughAuditSpy struct {
	mu    sync.Mutex
	calls [][]byte
}

func (a *passthroughAuditSpy) Publish(context.Context, string, []byte) error { return nil }
func (a *passthroughAuditSpy) Enqueue(_ context.Context, _ string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	copied := make([]byte, len(data))
	copy(copied, data)
	a.calls = append(a.calls, copied)
	return nil
}
func (a *passthroughAuditSpy) Close() error { return nil }

// snapshot returns a copy of the captured payloads for inspection.
func (a *passthroughAuditSpy) snapshot() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([][]byte, len(a.calls))
	for i, c := range a.calls {
		out[i] = append([]byte(nil), c...)
	}
	return out
}

// newHandlerWithAudit returns the standard mock-handler trio plus a
// spy-backed audit.Writer wired into h.audit. Falls through the same
// nil-pool guard as newMockHandler.
func newHandlerWithAudit(t *testing.T, hub HubConfigChanger) (pgxmock.PgxPoolIface, *Handler, *passthroughAuditSpy) {
	t.Helper()
	mock, h := newMockHandler(t, hub)
	spy := &passthroughAuditSpy{}
	h.audit = audit.NewWriter(spy, "nexus.event.admin-audit", h.logger)
	return mock, h, spy
}

// firstAuditEntry decodes the first captured audit payload and fails
// the test if the spy did not record exactly one entry — the audit
// emit must be deterministic, not "best effort fires sometimes".
func firstAuditEntry(t *testing.T, spy *passthroughAuditSpy) mq.AdminAuditMessage {
	t.Helper()
	calls := spy.snapshot()
	if len(calls) != 1 {
		t.Fatalf("FAILURE_MODE: expected exactly 1 audit emit; got %d", len(calls))
	}
	var msg mq.AdminAuditMessage
	if err := json.Unmarshal(calls[0], &msg); err != nil {
		t.Fatalf("decode admin audit message: %v", err)
	}
	return msg
}

// TestPutGlobal_EmitsAudit locks the contract that PutGlobal publishes
// an admin-audit message with entityType=passthrough,
// action=emergency-enable, entityId="global". BeforeState is omitted
// when no prior row exists; AfterState carries the new bypass flags +
// expiresAt + reason.
func TestPutGlobal_EmitsAudit(t *testing.T) {
	hub := &fakeHub{}
	mock, h, spy := newHandlerWithAudit(t, hub)
	// Prior-state read: no row.
	mock.ExpectQuery(`FROM gateway_passthrough_config_global WHERE id = \$1`).
		WithArgs("singleton").
		WillReturnError(noRowsErr())
	mock.ExpectExec(`INSERT INTO gateway_passthrough_config_global`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectEmptyAssembleBlob(mock)

	c, rec := echoCtxWithAdminAuth(http.MethodPut, "/passthrough/global", validBody(t))
	if err := h.PutGlobal(c); err != nil {
		t.Fatalf("PutGlobal err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}

	msg := firstAuditEntry(t, spy)
	if msg.EntityType != "passthrough" {
		t.Errorf("FAILURE_MODE: EntityType = %q; want passthrough", msg.EntityType)
	}
	if msg.Action != "emergency-enable" {
		t.Errorf("FAILURE_MODE: Action = %q; want emergency-enable", msg.Action)
	}
	if msg.EntityID != "global" {
		t.Errorf("FAILURE_MODE: EntityID = %q; want global", msg.EntityID)
	}
	if msg.ActorID != "admin-1" {
		t.Errorf("ActorID = %q; want admin-1", msg.ActorID)
	}
	// AfterState carries the validated payload — bypassHooks=true was set
	// in validBody.
	after, ok := msg.AfterState.(map[string]any)
	if !ok {
		t.Fatalf("FAILURE_MODE: AfterState type = %T; want map[string]any", msg.AfterState)
	}
	if after["bypassHooks"] != true {
		t.Errorf("AfterState.bypassHooks = %v; want true", after["bypassHooks"])
	}
	if after["enabled"] != true {
		t.Errorf("AfterState.enabled = %v; want true", after["enabled"])
	}
	if !strings.Contains(after["reason"].(string), "incident") {
		t.Errorf("AfterState.reason = %v; want substring 'incident'", after["reason"])
	}
	// BeforeState should be nil/empty when no prior row existed.
	if msg.BeforeState != nil {
		t.Errorf("BeforeState = %v; want nil for first-write", msg.BeforeState)
	}
}

// TestPutAdapter_EmitsAuditWithBeforeState locks the contract that
// PutAdapter snapshots the prior row into BeforeState when an existing
// adapter passthrough row is overwritten.
func TestPutAdapter_EmitsAuditWithBeforeState(t *testing.T) {
	hub := &fakeHub{}
	mock, h, spy := newHandlerWithAudit(t, hub)
	// Prior-state read: a row exists with bypassCache=true and a reason.
	priorReason := "prior reason text that is long enough"
	mock.ExpectQuery(`FROM gateway_passthrough_config_adapter WHERE adapter_type = \$1`).
		WithArgs("anthropic").
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "reason"}).
			AddRow(true, []byte(`{"bypassHooks":false,"bypassCache":true,"bypassNormalize":false}`), nil, &priorReason))
	mock.ExpectExec(`INSERT INTO gateway_passthrough_config_adapter`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectEmptyAssembleBlob(mock)

	c, rec := echoCtxWithAdminAuth(http.MethodPut, "/passthrough/adapter/anthropic", validBody(t))
	withParam(c, "adapter_type", "anthropic")
	if err := h.PutAdapter(c); err != nil {
		t.Fatalf("PutAdapter err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}

	msg := firstAuditEntry(t, spy)
	if msg.EntityID != "anthropic" {
		t.Errorf("EntityID = %q; want anthropic", msg.EntityID)
	}
	if msg.Action != "emergency-enable" {
		t.Errorf("Action = %q; want emergency-enable", msg.Action)
	}
	before, ok := msg.BeforeState.(map[string]any)
	if !ok {
		t.Fatalf("FAILURE_MODE: BeforeState type = %T; want map[string]any (prior row should snapshot)", msg.BeforeState)
	}
	if before["bypassCache"] != true {
		t.Errorf("BeforeState.bypassCache = %v; want true (from prior row)", before["bypassCache"])
	}
	if before["bypassHooks"] != false {
		t.Errorf("BeforeState.bypassHooks = %v; want false (from prior row)", before["bypassHooks"])
	}
	if before["reason"] != priorReason {
		t.Errorf("BeforeState.reason = %v; want %q", before["reason"], priorReason)
	}
}

// TestDeleteAdapter_EmitsAuditWithBeforeStateOnly locks the contract
// that DeleteAdapter publishes an audit entry carrying BeforeState
// (snapshotted from the now-deleted row) and no AfterState. Verb is
// "write" (the IAM gate on DELETE).
func TestDeleteAdapter_EmitsAuditWithBeforeStateOnly(t *testing.T) {
	hub := &fakeHub{}
	mock, h, spy := newHandlerWithAudit(t, hub)
	// Prior-state read.
	mock.ExpectQuery(`FROM gateway_passthrough_config_adapter WHERE adapter_type = \$1`).
		WithArgs("anthropic").
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "reason"}).
			AddRow(true, []byte(`{"bypassHooks":true}`), nil, (*string)(nil)))
	mock.ExpectExec(`DELETE FROM gateway_passthrough_config_adapter`).
		WithArgs("anthropic").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
	expectEmptyAssembleBlob(mock)

	c, rec := echoCtxWithAdminAuth(http.MethodDelete, "/passthrough/adapter/anthropic", "")
	withParam(c, "adapter_type", "anthropic")
	if err := h.DeleteAdapter(c); err != nil {
		t.Fatalf("DeleteAdapter err: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}

	msg := firstAuditEntry(t, spy)
	if msg.Action != "write" {
		t.Errorf("FAILURE_MODE: Action = %q; want write (DELETE uses VerbWrite per IAM gate)", msg.Action)
	}
	if msg.EntityID != "anthropic" {
		t.Errorf("EntityID = %q; want anthropic", msg.EntityID)
	}
	if msg.AfterState != nil {
		t.Errorf("FAILURE_MODE: AfterState = %v; want nil on delete", msg.AfterState)
	}
	if msg.BeforeState == nil {
		t.Fatalf("FAILURE_MODE: BeforeState must capture the deleted row")
	}
}

// TestPutProvider_EmitsAudit smokes the provider tier — the same audit
// shape as global / adapter but EntityID is the provider UUID.
func TestPutProvider_EmitsAudit(t *testing.T) {
	hub := &fakeHub{}
	mock, h, spy := newHandlerWithAudit(t, hub)
	mock.ExpectQuery(`FROM gateway_passthrough_config_provider WHERE provider_id = \$1`).
		WithArgs("prov-uuid-1").
		WillReturnError(noRowsErr())
	mock.ExpectExec(`INSERT INTO gateway_passthrough_config_provider`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectEmptyAssembleBlob(mock)

	c, rec := echoCtxWithAdminAuth(http.MethodPut, "/passthrough/provider/prov-uuid-1", validBody(t))
	withParam(c, "provider_id", "prov-uuid-1")
	if err := h.PutProvider(c); err != nil {
		t.Fatalf("PutProvider err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}

	msg := firstAuditEntry(t, spy)
	if msg.EntityType != "passthrough" || msg.Action != "emergency-enable" || msg.EntityID != "prov-uuid-1" {
		t.Errorf("FAILURE_MODE: entry = (%q, %q, %q); want (passthrough, emergency-enable, prov-uuid-1)",
			msg.EntityType, msg.Action, msg.EntityID)
	}
}

// TestDeleteProvider_EmitsAudit smokes the provider-delete audit.
func TestDeleteProvider_EmitsAudit(t *testing.T) {
	hub := &fakeHub{}
	mock, h, spy := newHandlerWithAudit(t, hub)
	mock.ExpectQuery(`FROM gateway_passthrough_config_provider WHERE provider_id = \$1`).
		WithArgs("prov-uuid-1").
		WillReturnRows(pgxmock.NewRows([]string{"enabled", "config", "expires_at", "reason"}).
			AddRow(true, []byte(`{}`), nil, (*string)(nil)))
	mock.ExpectExec(`DELETE FROM gateway_passthrough_config_provider`).
		WithArgs("prov-uuid-1").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
	expectEmptyAssembleBlob(mock)

	c, rec := echoCtxWithAdminAuth(http.MethodDelete, "/passthrough/provider/prov-uuid-1", "")
	withParam(c, "provider_id", "prov-uuid-1")
	if err := h.DeleteProvider(c); err != nil {
		t.Fatalf("DeleteProvider err: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}

	msg := firstAuditEntry(t, spy)
	if msg.Action != "write" {
		t.Errorf("FAILURE_MODE: Action = %q; want write", msg.Action)
	}
	if msg.EntityID != "prov-uuid-1" {
		t.Errorf("EntityID = %q; want prov-uuid-1", msg.EntityID)
	}
}

// TestPutGlobal_NilAuditWriterDoesNotPanic locks the nil-Writer guard:
// when audit wiring is absent (test harness, partial Deps) the handler
// must still complete the user-visible mutation without panicking. The
// nil guard sits in h.emitAudit; without it the LogObserved call
// dereferences nil and crashes the request.
func TestPutGlobal_NilAuditWriterDoesNotPanic(t *testing.T) {
	hub := &fakeHub{}
	mock, h := newMockHandler(t, hub) // audit stays nil
	mock.ExpectQuery(`FROM gateway_passthrough_config_global WHERE id = \$1`).
		WithArgs("singleton").
		WillReturnError(noRowsErr())
	mock.ExpectExec(`INSERT INTO gateway_passthrough_config_global`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectEmptyAssembleBlob(mock)

	c, rec := echoCtxWithAdminAuth(http.MethodPut, "/passthrough/global", validBody(t))
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("FAILURE_MODE: nil audit panicked: %v", r)
		}
	}()
	if err := h.PutGlobal(c); err != nil {
		t.Fatalf("PutGlobal err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
}
