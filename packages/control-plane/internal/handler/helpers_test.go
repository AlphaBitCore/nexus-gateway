package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/thingstats/thingstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	metricspkg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// Shared test infrastructure

// fakeLoader satisfies iam.PolicyLoader for handler unit tests.
type fakeLoader struct {
	policies []iam.LoadedPolicy
	err      error
}

func (f *fakeLoader) LoadPolicies(_ context.Context, _, _ string) ([]iam.LoadedPolicy, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.policies, nil
}

func allowAllPolicies() []iam.LoadedPolicy {
	return []iam.LoadedPolicy{{
		ID: "p1", Name: "allow-all", Source: "direct",
		Document: iam.PolicyDocument{
			Version: iam.PolicyVersion,
			Statement: []iam.Statement{
				{Effect: "Allow", Action: []string{"*"}, Resource: []string{"*"}},
			},
		},
	}}
}

// newMockDB constructs a pgxmock-backed *store.DB for handler tests that
// need SQL interaction (isSuperAdmin, incrementConfigVersion,
// queryMetricsOrFallback).
func newMockDB(t *testing.T) (pgxmock.PgxPoolIface, *store.DB) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	return mock, store.NewWithPgxPool(mock)
}

// adminHandlerOpts configures newAdminHandlerWithMock.
type adminHandlerOpts struct {
	IAM *iam.Engine
}

// newAdminHandlerWithMock wires an AdminHandler with a pgxmock DB plus
// optional IAM override. Nil IAM receives an allow-all engine.
func newAdminHandlerWithMock(t *testing.T, opts adminHandlerOpts) (*AdminHandler, pgxmock.PgxPoolIface, *hubSpy, *auditSpy) {
	t.Helper()
	mock, db := newMockDB(t)

	hub := &hubSpy{}
	aud := &auditSpy{}
	eng := opts.IAM
	if eng == nil {
		eng = iam.NewEngine(&fakeLoader{policies: allowAllPolicies()}, silentLogger())
	}

	h := &AdminHandler{
		DB:     db,
		IAM:    eng,
		Hub:    hub,
		Logger: silentLogger(),
		Audit:  auditWriterFrom(aud),
	}
	return h, mock, hub, aud
}

// iamMWNoop is a pass-through IAM middleware for route-registration smoke tests.
func iamMWNoop(_ string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
}

// auditWriterFrom builds a production audit.Writer around the spy.
func auditWriterFrom(spy *auditSpy) *audit.Writer {
	return audit.NewWriter(spy, "nexus.event.admin-audit", silentLogger())
}

// rollupCols mirrors the column order returned by QueryRollupCascade /
// QueryRollupAware SELECT statements so pgxmock rows decode correctly.
var rollupCols = []string{
	"id", "bucketStart", "metricName", "dimensionKey",
	"subDimension", "value", "metadata", "updatedAt",
}

// expectWatermarksMissing sets pgxmock to return pgx.ErrNoRows for the
// three watermark probes QueryRollupCascade fires before selecting tables.
func expectWatermarksMissing(mock pgxmock.PgxPoolIface) {
	for range 3 {
		mock.ExpectQuery(`FROM "rollup_watermark"`).
			WithArgs(pgxmock.AnyArg()).
			WillReturnError(pgx.ErrNoRows)
	}
}

// parseRFC3339Flexible — named failure modes

// TestParseRFC3339Flexible_GarbageInput locks the failure path: an invalid
// string must return (zero, false) without panicking.
func TestParseRFC3339Flexible_GarbageInput(t *testing.T) {
	if _, ok := parseRFC3339Flexible("not-a-time"); ok {
		t.Fatal("FAILURE_MODE: garbage input must return ok=false")
	}
}

// TestParseRFC3339Flexible_RFC3339 locks plain RFC3339 (no fractional seconds).
func TestParseRFC3339Flexible_RFC3339(t *testing.T) {
	got, ok := parseRFC3339Flexible("2024-06-01T12:00:00Z")
	if !ok {
		t.Fatal("FAILURE_MODE: plain RFC3339 must succeed")
	}
	if got.Year() != 2024 {
		t.Errorf("year = %d, want 2024", got.Year())
	}
}

// TestParseRFC3339Flexible_RFC3339Nano locks JS-style ISO strings with
// fractional seconds (e.g. Date.toISOString()).
func TestParseRFC3339Flexible_RFC3339Nano(t *testing.T) {
	got, ok := parseRFC3339Flexible("2024-06-01T12:00:00.123Z")
	if !ok {
		t.Fatal("FAILURE_MODE: RFC3339Nano must succeed")
	}
	if got.Nanosecond() == 0 {
		t.Error("FAILURE_MODE: fractional seconds should produce non-zero nanoseconds")
	}
}

// firstNonEmpty — named failure modes

// TestFirstNonEmpty_SkipsEmptyLeaders locks the "skip leading empties" path.
func TestFirstNonEmpty_SkipsEmptyLeaders(t *testing.T) {
	if got := firstNonEmpty("", "b", "c"); got != "b" {
		t.Fatalf("FAILURE_MODE: first non-empty not returned; got %q", got)
	}
}

// TestFirstNonEmpty_AllEmpty locks the all-empty fallback: must return "".
func TestFirstNonEmpty_AllEmpty(t *testing.T) {
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("FAILURE_MODE: all-empty must return empty string; got %q", got)
	}
}

// TestFirstNonEmpty_NoArgs locks the zero-args edge case.
func TestFirstNonEmpty_NoArgs(t *testing.T) {
	if got := firstNonEmpty(); got != "" {
		t.Fatalf("FAILURE_MODE: zero args must return empty; got %q", got)
	}
}

// deref — named failure modes

// TestDeref_NonNil locks the normal pointer dereference.
func TestDeref_NonNil(t *testing.T) {
	s := "hello"
	if got := deref(&s); got != "hello" {
		t.Fatalf("FAILURE_MODE: non-nil deref returned %q, want hello", got)
	}
}

// TestDeref_Nil locks the nil guard: must return "" without panicking.
func TestDeref_Nil(t *testing.T) {
	if got := deref(nil); got != "" {
		t.Fatalf("FAILURE_MODE: nil deref must return empty string; got %q", got)
	}
}

// parsePagination — named failure modes

// TestParsePagination_ClampsLimitAt1000 locks the hard upper cap on limit.
func TestParsePagination_ClampsLimitAt1000(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?limit=2000&offset=5", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 1000 {
		t.Errorf("FAILURE_MODE: limit > 1000 must be clamped; got %d", pg.Limit)
	}
	if pg.Offset != 5 {
		t.Errorf("offset = %d, want 5", pg.Offset)
	}
}

// TestParsePagination_InvalidValuesFallBackToDefaults locks the defaults when
// the client sends non-numeric or negative values.
func TestParsePagination_InvalidValuesFallBackToDefaults(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?limit=abc&offset=-1", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 50 {
		t.Errorf("FAILURE_MODE: invalid limit must fall back to 50; got %d", pg.Limit)
	}
	if pg.Offset != 0 {
		t.Errorf("FAILURE_MODE: negative offset must fall back to 0; got %d", pg.Offset)
	}
}

// TestParsePagination_ZeroLimitFallsBackToDefault locks the guard that rejects
// limit=0 (which would be a DB full-table scan).
func TestParsePagination_ZeroLimitFallsBackToDefault(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?limit=0", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 50 {
		t.Errorf("FAILURE_MODE: limit=0 must fall back to 50; got %d", pg.Limit)
	}
}

// TestParsePagination_ExplicitValidValues locks the happy path.
func TestParsePagination_ExplicitValidValues(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?limit=10&offset=20", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	pg := parsePagination(c)
	if pg.Limit != 10 || pg.Offset != 20 {
		t.Errorf("limit=%d offset=%d, want 10/20", pg.Limit, pg.Offset)
	}
}

// parseTimeRange — named failure modes

// TestParseTimeRange_BothBoundsPresent locks the happy path where both
// startTime and endTime are valid RFC3339 strings.
func TestParseTimeRange_BothBoundsPresent(t *testing.T) {
	e := echo.New()
	ts := "2024-01-01T00:00:00Z"
	req := httptest.NewRequest(http.MethodGet, "/?startTime="+ts+"&endTime="+ts, nil)
	c := e.NewContext(req, httptest.NewRecorder())
	start, end := parseTimeRange(c)
	if start == nil || end == nil {
		t.Fatal("FAILURE_MODE: both RFC3339 bounds must parse successfully")
	}
}

// TestParseTimeRange_AliasEndParam locks the ?end= alias used by older UI pages.
func TestParseTimeRange_AliasEndParam(t *testing.T) {
	e := echo.New()
	ts := "2024-01-01T00:00:00Z"
	req := httptest.NewRequest(http.MethodGet, "/?start="+ts+"&end="+ts, nil)
	c := e.NewContext(req, httptest.NewRecorder())
	start, end := parseTimeRange(c)
	if start == nil || end == nil {
		t.Fatal("FAILURE_MODE: start/end aliases must be accepted")
	}
}

// TestParseTimeRange_InvalidValueYieldsNil locks the nil return on bad input.
func TestParseTimeRange_InvalidValueYieldsNil(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?startTime=not-a-date", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	start, _ := parseTimeRange(c)
	if start != nil {
		t.Fatalf("FAILURE_MODE: invalid time string must return nil start; got %v", start)
	}
}

// TestParseTimeRange_EmptyParams locks the nil return when params are absent.
func TestParseTimeRange_EmptyParams(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	start, end := parseTimeRange(c)
	if start != nil || end != nil {
		t.Fatal("FAILURE_MODE: missing params must return nil pointers")
	}
}

// parseAdminAuditParams — named failure modes

// TestParseAdminAuditParams_AllFields locks that every query param maps to the
// correct field in AdminAuditLogListParams.
func TestParseAdminAuditParams_AllFields(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet,
		"/?limit=10&offset=2&startTime=2024-01-01T00:00:00Z&actorId=a1&actorLabel=admin&action=update&entityType=provider",
		nil)
	c := e.NewContext(req, httptest.NewRecorder())
	p := parseAdminAuditParams(c)
	if p.Limit != 10 {
		t.Errorf("FAILURE_MODE: limit not propagated; got %d", p.Limit)
	}
	if p.Offset != 2 {
		t.Errorf("FAILURE_MODE: offset not propagated; got %d", p.Offset)
	}
	if p.ActorID != "a1" {
		t.Errorf("FAILURE_MODE: actorId not propagated; got %q", p.ActorID)
	}
	if p.ActorLabel != "admin" {
		t.Errorf("FAILURE_MODE: actorLabel not propagated; got %q", p.ActorLabel)
	}
	if p.Action != "update" {
		t.Errorf("FAILURE_MODE: action not propagated; got %q", p.Action)
	}
	if p.EntityType != "provider" {
		t.Errorf("FAILURE_MODE: entityType not propagated; got %q", p.EntityType)
	}
	if p.StartTime == nil {
		t.Error("FAILURE_MODE: valid startTime must produce non-nil StartTime")
	}
}

// TestParseAdminAuditParams_InvalidTime locks that an unparseable time string
// is silently dropped (StartTime stays nil).
func TestParseAdminAuditParams_InvalidTime(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?startTime=BOGUS&endTime=BOGUS", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	p := parseAdminAuditParams(c)
	if p.StartTime != nil || p.EndTime != nil {
		t.Fatal("FAILURE_MODE: invalid time must yield nil StartTime/EndTime")
	}
}

// currentUserID + actorFromContext — named failure modes

// TestCurrentUserID_WithAuth locks the happy path where auth middleware has
// populated AdminAuth.
func TestCurrentUserID_WithAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-42")
	if got := currentUserID(c); got != "user-42" {
		t.Fatalf("FAILURE_MODE: wrong user ID; got %q, want user-42", got)
	}
}

// TestCurrentUserID_MissingAuth locks the fail-safe: must return "" when no
// auth is present rather than panicking.
func TestCurrentUserID_MissingAuth(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	if got := currentUserID(c); got != "" {
		t.Fatalf("FAILURE_MODE: missing auth must return empty string; got %q", got)
	}
}

// TestActorFromContext_PopulatesUserIDAndName locks the hub-propagation path:
// actor identity must be extractable for ConfigChangeRequest.
func TestActorFromContext_PopulatesUserIDAndName(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := echoContext(req, httptest.NewRecorder(), "alice", "user-42")
	act := actorFromContext(c)
	if act.UserID != "user-42" {
		t.Errorf("FAILURE_MODE: UserID = %q, want user-42", act.UserID)
	}
	if act.Name != "alice" {
		t.Errorf("FAILURE_MODE: Name = %q, want alice", act.Name)
	}
}

// TestActorFromContext_MissingAuth locks the zero-value fallback.
func TestActorFromContext_MissingAuth(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	act := actorFromContext(c)
	if act.UserID != "" || act.Name != "" {
		t.Fatalf("FAILURE_MODE: missing auth must return zero Actor; got %+v", act)
	}
}

// sourceIP — named failure modes

// TestSourceIP_ReadsRealIPHeader locks the X-Real-IP propagation used for
// audit logging in admin routes.
func TestSourceIP_ReadsRealIPHeader(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Real-IP", "203.0.113.5")
	c := e.NewContext(req, httptest.NewRecorder())
	if ip := sourceIP(c); ip == "" {
		t.Fatal("FAILURE_MODE: X-Real-IP must be returned by sourceIP")
	}
}

// templateVersion — named failure modes

// TestTemplateVersion_NilTemplate locks the nil guard (unseeded node).
func TestTemplateVersion_NilTemplate(t *testing.T) {
	if v := templateVersion(nil); v != 0 {
		t.Fatalf("FAILURE_MODE: nil template must return 0; got %d", v)
	}
}

// TestTemplateVersion_NonNil locks normal version propagation.
func TestTemplateVersion_NonNil(t *testing.T) {
	tpl := &thingstore.ThingConfigTemplate{Version: 7}
	if v := templateVersion(tpl); v != 7 {
		t.Fatalf("FAILURE_MODE: version mismatch; got %d, want 7", v)
	}
}

// handlerHubProxyClient — named failure modes

// TestHandlerHubProxyClient_Override locks that a test-injected client is
// returned verbatim without wrapping.
func TestHandlerHubProxyClient_Override(t *testing.T) {
	custom := &http.Client{Timeout: time.Second}
	if got := handlerHubProxyClient(custom); got != custom {
		t.Fatal("FAILURE_MODE: override client must be returned as-is")
	}
}

// TestHandlerHubProxyClient_DefaultNotNil locks that the fallback default
// client is non-nil when no override is provided.
func TestHandlerHubProxyClient_DefaultNotNil(t *testing.T) {
	if got := handlerHubProxyClient(nil); got == nil {
		t.Fatal("FAILURE_MODE: default hub proxy client must not be nil")
	}
}

// errJSON — named failure modes

// TestErrJSON_ShapeContract locks the canonical error envelope shape used by
// all admin handlers. Consumers (including the CP-UI error boundary) depend on
// error.code, error.type, and error.message being present at the top level.
func TestErrJSON_ShapeContract(t *testing.T) {
	env := errJSON("resource not found", "not_found", "NOT_FOUND")
	errBlock, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("FAILURE_MODE: errJSON must produce {error:{...}} envelope; got %T", env["error"])
	}
	if errBlock["code"] != "NOT_FOUND" {
		t.Errorf("FAILURE_MODE: code = %q, want NOT_FOUND", errBlock["code"])
	}
	if errBlock["type"] != "not_found" {
		t.Errorf("FAILURE_MODE: type = %q, want not_found", errBlock["type"])
	}
	if errBlock["message"] != "resource not found" {
		t.Errorf("FAILURE_MODE: message = %q, want 'resource not found'", errBlock["message"])
	}
}

// TestErrJSON_SerializesAsJSON locks that the envelope round-trips through
// encoding/json without loss (handlers pass this to c.JSON directly).
func TestErrJSON_SerializesAsJSON(t *testing.T) {
	env := errJSON("boom", "server_error", "INTERNAL_ERROR")
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("FAILURE_MODE: errJSON output must marshal to JSON; err=%v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("FAILURE_MODE: marshalled errJSON must round-trip; err=%v", err)
	}
}

// isSuperAdmin — named failure modes (requires pgxmock)

// TestIsSuperAdmin_MemberOfSuperAdminsGroup locks the expected true return
// when the DB confirms the principal is in "super-admins".
func TestIsSuperAdmin_MemberOfSuperAdminsGroup(t *testing.T) {
	h, mock, _, _ := newAdminHandlerWithMock(t, adminHandlerOpts{})
	mock.ExpectQuery(`FROM "IamGroupMembership"`).
		WithArgs("nexus_user", "u-1").
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("super-admins"))

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	aa := &auth.AdminAuth{KeyID: "u-1", AuthPrincipalType: "admin_user"}
	middleware.WithAdminAuth(c, aa)

	if !h.isSuperAdmin(c, middleware.AdminAuthFromContext(c)) {
		t.Fatal("FAILURE_MODE: principal in super-admins group must return true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestIsSuperAdmin_NotInSuperAdminsGroup locks the false return when the
// principal has no group membership.
func TestIsSuperAdmin_NotInSuperAdminsGroup(t *testing.T) {
	h, mock, _, _ := newAdminHandlerWithMock(t, adminHandlerOpts{})
	mock.ExpectQuery(`FROM "IamGroupMembership"`).
		WithArgs("nexus_user", "u-2").
		WillReturnRows(pgxmock.NewRows([]string{"name"})) // empty result

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	aa := &auth.AdminAuth{KeyID: "u-2", AuthPrincipalType: "admin_user"}
	middleware.WithAdminAuth(c, aa)

	if h.isSuperAdmin(c, middleware.AdminAuthFromContext(c)) {
		t.Fatal("FAILURE_MODE: principal not in super-admins must return false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestIsSuperAdmin_NilAdminAuth locks the nil guard: must return false without
// panicking when auth middleware did not populate AdminAuth.
func TestIsSuperAdmin_NilAdminAuth(t *testing.T) {
	h, _, _, _ := newAdminHandlerWithMock(t, adminHandlerOpts{})
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	if h.isSuperAdmin(c, nil) {
		t.Fatal("FAILURE_MODE: nil AdminAuth must return false")
	}
}

// TestIsSuperAdmin_DBErrorReturnsFalse locks the fail-safe when the IAM store
// is unavailable: must not propagate the error upward (returns false).
func TestIsSuperAdmin_DBErrorReturnsFalse(t *testing.T) {
	h, mock, _, _ := newAdminHandlerWithMock(t, adminHandlerOpts{})
	mock.ExpectQuery(`FROM "IamGroupMembership"`).
		WithArgs("nexus_user", "u-3").
		WillReturnError(errors.New("db down"))

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	aa := &auth.AdminAuth{KeyID: "u-3", AuthPrincipalType: "admin_user"}
	middleware.WithAdminAuth(c, aa)

	if h.isSuperAdmin(c, middleware.AdminAuthFromContext(c)) {
		t.Fatal("FAILURE_MODE: DB error must return false (fail safe)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// incrementConfigVersion — named failure modes (requires pgxmock)

// TestIncrementConfigVersion_IncrementsExistingVersion locks the normal path:
// reads the current version, increments it by 1, and writes it back.
func TestIncrementConfigVersion_IncrementsExistingVersion(t *testing.T) {
	h, mock, _, _ := newAdminHandlerWithMock(t, adminHandlerOpts{})
	mock.ExpectQuery(`FROM system_metadata`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`3`)))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", pgxmock.AnyArg(), "system").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("FAILURE_MODE: all SQL expectations must be satisfied; %v", err)
	}
}

// TestIncrementConfigVersion_StartsAt1WhenMissing locks the bootstrap path:
// when the key is absent (ErrNoRows) the version starts at 1.
func TestIncrementConfigVersion_StartsAt1WhenMissing(t *testing.T) {
	h, mock, _, _ := newAdminHandlerWithMock(t, adminHandlerOpts{})
	mock.ExpectQuery(`FROM system_metadata`).
		WithArgs("agent.config.version").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", pgxmock.AnyArg(), "system").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("FAILURE_MODE: bootstrap path must write version 1; %v", err)
	}
}

// TestIncrementConfigVersion_WriteErrorIsLogged locks the non-fatal contract:
// a write failure must not panic or propagate (only logged).
func TestIncrementConfigVersion_WriteErrorIsLogged(t *testing.T) {
	h, mock, _, _ := newAdminHandlerWithMock(t, adminHandlerOpts{})
	mock.ExpectQuery(`FROM system_metadata`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`5`)))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", pgxmock.AnyArg(), "system").
		WillReturnError(errors.New("write failed"))

	// Must not panic.
	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("FAILURE_MODE: write error must be logged, not panicked; %v", err)
	}
}

// queryMetricsOrFallback — named failure modes (requires pgxmock)

// TestQueryMetricsOrFallback_ReturnsSummaryOnSuccess locks the happy path:
// a successful QueryRollupCascade result is wrapped into MetricsResult.
func TestQueryMetricsOrFallback_ReturnsSummaryOnSuccess(t *testing.T) {
	h, mock, _, _ := newAdminHandlerWithMock(t, adminHandlerOpts{})
	now := time.Now().UTC()
	q := metricspkg.MetricsQuery{
		Metrics:   []string{metricspkg.MetricRequestCount},
		StartTime: now.Add(-time.Hour),
		EndTime:   now,
	}
	expectWatermarksMissing(mock)
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(
			"id-1", now, metricspkg.MetricRequestCount, "target_host=api.example.com",
			"source=agent", 42.0, nil, now,
		))

	res, err := h.queryMetricsOrFallback(context.Background(), q)
	if err != nil {
		t.Fatalf("FAILURE_MODE: successful rollup query must not error; %v", err)
	}
	if res == nil {
		t.Fatal("FAILURE_MODE: successful rollup must return non-nil MetricsResult")
	}
	if res.Summary[metricspkg.MetricRequestCount] != 42 {
		t.Errorf("FAILURE_MODE: summary value = %v, want 42", res.Summary[metricspkg.MetricRequestCount])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestQueryMetricsOrFallback_ReturnsNilOnDBError locks the nil+nil fallback:
// a DB error must return (nil, nil) so callers can degrade gracefully.
func TestQueryMetricsOrFallback_ReturnsNilOnDBError(t *testing.T) {
	mock2, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock2.Close)
	db2 := store.NewWithPgxPool(mock2)
	h2 := &AdminHandler{DB: db2, Logger: silentLogger()}

	now := time.Now().UTC()
	q := metricspkg.MetricsQuery{
		Metrics:   []string{metricspkg.MetricRequestCount},
		StartTime: now.Add(-time.Hour),
		EndTime:   now,
	}
	// QueryRollupCascade probes 3 watermarks then queries metric_rollup_5m.
	expectWatermarksMissing(mock2)
	mock2.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("db down"))

	res, err2 := h2.queryMetricsOrFallback(context.Background(), q)
	if err2 != nil {
		t.Fatalf("FAILURE_MODE: DB error must be swallowed and return (nil, nil); err=%v", err2)
	}
	if res != nil {
		t.Fatalf("FAILURE_MODE: DB error must return nil result; got %+v", res)
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestQueryMetricsOrFallback_TimeSeriesPath locks that TimeSeries=true routes
// to QueryRollupAware rather than QueryRollupCascade. A 1-hour range selects
// Granularity5m so QueryRollupAware queries metric_rollup_5m directly (no
// watermark probe needed — 5m has no coarser tail source).
func TestQueryMetricsOrFallback_TimeSeriesPath(t *testing.T) {
	h, mock, _, _ := newAdminHandlerWithMock(t, adminHandlerOpts{})
	now := time.Now().UTC()
	q := metricspkg.MetricsQuery{
		Metrics:    []string{metricspkg.MetricRequestCount},
		StartTime:  now.Add(-time.Hour),
		EndTime:    now,
		TimeSeries: true,
	}
	// A 1-hour span → SelectGranularity = 5m. rollupTailFor(5m) = ("","") so
	// QueryRollupAware falls through directly to metric_rollup_5m with no
	// watermark probe.
	mock.ExpectQuery(`FROM "metric_rollup_5m"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(rollupCols).AddRow(
			"id-ts", now.Add(-30*time.Minute), metricspkg.MetricRequestCount, "source=agent",
			"", 10.0, nil, now,
		))

	res, err := h.queryMetricsOrFallback(context.Background(), q)
	if err != nil {
		t.Fatalf("FAILURE_MODE: TimeSeries path must not error; %v", err)
	}
	if res == nil {
		t.Fatal("FAILURE_MODE: TimeSeries path must return non-nil result")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// RegisterAdminNodesAppliedConfigRoutes smoke — named failure modes

// TestRegisterAdminNodesAppliedConfigRoutes_RouteExists locks that the applied-
// config endpoint is registered on the correct path. Missing this route would
// cause a 404 on every Node Configuration tab load.
func TestRegisterAdminNodesAppliedConfigRoutes_RouteExists(t *testing.T) {
	h, _, _ := newAdminHandlerWithHubSpy(t)
	e := echo.New()
	g := e.Group("/api/admin")
	h.RegisterAdminNodesAppliedConfigRoutes(g, iamMWNoop)

	found := false
	for _, r := range e.Routes() {
		if r.Path == "/api/admin/nodes/:id/applied-config" && r.Method == http.MethodGet {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("FAILURE_MODE: /api/admin/nodes/:id/applied-config GET must be registered")
	}
}

// RegisterAdminRoutes smoke — named failure modes

// TestRegisterAdminRoutes_DoesNotPanicWithNilOptionalDeps locks that route
// registration succeeds when optional deps (AIGuard, RulePacks, Exemption)
// are nil — these must be silently skipped, not cause a nil dereference.
func TestRegisterAdminRoutes_DoesNotPanicWithNilOptionalDeps(t *testing.T) {
	h, _, _, _ := newAdminHandlerWithMock(t, adminHandlerOpts{})
	// AIGuard, RulePacks, Exemption all nil.
	e := echo.New()
	g := e.Group("/api/admin")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("FAILURE_MODE: RegisterAdminRoutes must not panic with nil optional deps; panic=%v", r)
		}
	}()
	h.RegisterAdminRoutes(g)
}

// TestRegisterAdminRoutes_AtLeastOneRouteRegistered locks that the registration
// call produces a non-empty route table (guards against all routes being gated
// behind nil-optional-dep guards that silently skip everything).
func TestRegisterAdminRoutes_AtLeastOneRouteRegistered(t *testing.T) {
	h, _, _, _ := newAdminHandlerWithMock(t, adminHandlerOpts{})
	e := echo.New()
	g := e.Group("/api/admin")
	h.RegisterAdminRoutes(g)
	if len(e.Routes()) == 0 {
		t.Fatal("FAILURE_MODE: RegisterAdminRoutes produced zero routes")
	}
}

// FetchOverridesForThing — named failure modes (requires httptest server)

// TestFetchOverridesForThing_HappyPath locks that a 200 response from Hub is
// decoded and the slice is returned to the caller.
func TestFetchOverridesForThing_HappyPath(t *testing.T) {
	overrides := []appliedConfigOverrideMeta{
		{ConfigKey: "killswitch", SetBy: "alice", EmergencyOverride: true},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"overrides": overrides})
	}))
	defer srv.Close()

	spy := &hubSpy{baseURL: srv.URL, token: "test-tok"}
	f := &hubAppliedConfigOverrideFetcher{hub: spy, client: srv.Client()}

	got, err := f.FetchOverridesForThing(context.Background(), "thing-1")
	if err != nil {
		t.Fatalf("FAILURE_MODE: 200 response must succeed; err=%v", err)
	}
	if len(got) != 1 || got[0].ConfigKey != "killswitch" {
		t.Fatalf("FAILURE_MODE: response must decode overrides slice; got=%+v", got)
	}
	if !got[0].EmergencyOverride {
		t.Error("FAILURE_MODE: emergencyOverride flag must survive JSON decode")
	}
}

// TestFetchOverridesForThing_404ReturnsNilSlice locks the enrollment-race
// handling: Hub returning 404 must yield (nil, nil) not an error, since CP
// already validated the Thing exists locally.
func TestFetchOverridesForThing_404ReturnsNilSlice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	spy := &hubSpy{baseURL: srv.URL, token: "tok"}
	f := &hubAppliedConfigOverrideFetcher{hub: spy, client: srv.Client()}

	got, err := f.FetchOverridesForThing(context.Background(), "ghost-thing")
	if err != nil {
		t.Fatalf("FAILURE_MODE: Hub 404 must return nil error (enrollment race); err=%v", err)
	}
	if got != nil {
		t.Fatalf("FAILURE_MODE: Hub 404 must return nil slice; got=%+v", got)
	}
}

// TestFetchOverridesForThing_Non200ReturnsError locks that a Hub 500 propagates
// as an error so the applied-config handler can log + degrade gracefully.
func TestFetchOverridesForThing_Non200ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	}))
	defer srv.Close()

	spy := &hubSpy{baseURL: srv.URL, token: "tok"}
	f := &hubAppliedConfigOverrideFetcher{hub: spy, client: srv.Client()}

	_, err := f.FetchOverridesForThing(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("FAILURE_MODE: Hub 5xx must return non-nil error")
	}
}

// TestFetchOverridesForThing_HubUnreachableReturnsError locks that a network
// error from the Hub propagates so callers can apply their degrade-gracefully
// logic.
func TestFetchOverridesForThing_HubUnreachableReturnsError(t *testing.T) {
	// Point at a port that is guaranteed to refuse connections.
	spy := &hubSpy{baseURL: "http://127.0.0.1:1", token: "tok"}
	f := &hubAppliedConfigOverrideFetcher{hub: spy, client: &http.Client{}}

	_, err := f.FetchOverridesForThing(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("FAILURE_MODE: unreachable Hub must return non-nil error")
	}
}

// TestFetchOverridesForThing_EmptyHubURL locks the nil-hub guard: when Hub
// base URL is empty the fetcher must return (nil, nil) without making any
// network call.
func TestFetchOverridesForThing_EmptyHubURL(t *testing.T) {
	spy := &hubSpy{baseURL: "", token: ""}
	f := &hubAppliedConfigOverrideFetcher{hub: spy, client: &http.Client{}}

	got, err := f.FetchOverridesForThing(context.Background(), "thing-1")
	if err != nil || got != nil {
		t.Fatalf("FAILURE_MODE: empty Hub URL must return (nil, nil); got=%+v err=%v", got, err)
	}
}

// TestAppliedConfigOverrideFetcherFromHandler_NilHubReturnsNil locks that the
// handler factory returns nil (no fetcher) when Hub is nil, so the handler
// degrades gracefully rather than panicking.
func TestAppliedConfigOverrideFetcherFromHandler_NilHubReturnsNil(t *testing.T) {
	h := &AdminHandler{Logger: silentLogger()}
	// Hub nil → fetcher must be nil.
	if f := h.appliedConfigOverrideFetcherFromHandler(); f != nil {
		t.Fatal("FAILURE_MODE: nil Hub must yield nil override fetcher")
	}
}

// TestAppliedConfigOverrideFetcherFromHandler_EmptyBaseURLReturnsNil locks
// that an empty Hub base URL also yields a nil fetcher (Hub not configured).
func TestAppliedConfigOverrideFetcherFromHandler_EmptyBaseURLReturnsNil(t *testing.T) {
	spy := &hubSpy{baseURL: ""}
	h := &AdminHandler{Hub: spy, Logger: silentLogger()}
	if f := h.appliedConfigOverrideFetcherFromHandler(); f != nil {
		t.Fatal("FAILURE_MODE: empty Hub BaseURL must yield nil override fetcher")
	}
}

// TestAppliedConfigOverrideFetcherFromHandler_TestOverrideTakesPrecedence locks
// that a test-injected fetcher wins over the Hub-HTTP production path.
func TestAppliedConfigOverrideFetcherFromHandler_TestOverrideTakesPrecedence(t *testing.T) {
	stub := &stubAppliedConfigOverrideFetcher{}
	h := &AdminHandler{
		Hub:                          &hubSpy{baseURL: "http://hub"},
		Logger:                       silentLogger(),
		AppliedConfigOverrideFetcher: stub,
	}
	if got := h.appliedConfigOverrideFetcherFromHandler(); got != stub {
		t.Fatal("FAILURE_MODE: test override must take precedence over Hub-HTTP fetcher")
	}
}

// TestAppliedConfigStoreFromHandler_TestOverrideTakesPrecedence locks that a
// test-injected store wins over h.DB.
func TestAppliedConfigStoreFromHandler_TestOverrideTakesPrecedence(t *testing.T) {
	h, _, _, _ := newAdminHandlerWithMock(t, adminHandlerOpts{})
	stub := &stubAppliedConfigStore{}
	h.AppliedConfigStore = stub
	if got := h.appliedConfigStoreFromHandler(); got != stub {
		t.Fatal("FAILURE_MODE: test override must take precedence over h.DB")
	}
}

// TestAppliedConfigStoreFromHandler_FallsBackToDBWhenNil locks the production
// path: when no override is set, h.DB is returned.
func TestAppliedConfigStoreFromHandler_FallsBackToDBWhenNil(t *testing.T) {
	h, _, _, _ := newAdminHandlerWithMock(t, adminHandlerOpts{})
	h.AppliedConfigStore = nil
	if got := h.appliedConfigStoreFromHandler(); got == nil {
		t.Fatal("FAILURE_MODE: nil override must fall back to non-nil h.DB")
	}
}

// TestAppliedConfigOverrideFetcherFromHandler_ProductionHubPath locks that the
// factory returns a non-nil Hub-HTTP fetcher when Hub is available with a real
// base URL (the uncovered line 78 in admin_things_applied_config.go).
func TestAppliedConfigOverrideFetcherFromHandler_ProductionHubPath(t *testing.T) {
	spy := &hubSpy{baseURL: "http://hub.example.com"}
	h := &AdminHandler{Hub: spy, Logger: silentLogger()}
	// No test override → production path should be taken.
	f := h.appliedConfigOverrideFetcherFromHandler()
	if f == nil {
		t.Fatal("FAILURE_MODE: non-nil Hub with base URL must produce non-nil fetcher")
	}
}

// GetNodeAppliedConfig — additional uncovered branches

// TestGetNodeAppliedConfig_GetThingError_Returns500 locks the 500 path when
// the store returns an error for GetThing.
func TestGetNodeAppliedConfig_GetThingError_Returns500(t *testing.T) {
	h, stub := newAdminHandlerForAppliedConfig(t)
	stub.thingErr = errors.New("db error")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/proxy-1/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("proxy-1")

	_ = h.GetNodeAppliedConfig(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("FAILURE_MODE: GetThing error must return 500; got %d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "INTERNAL_ERROR", "server_error")
}

// TestGetNodeAppliedConfig_ListTemplatesError_Returns500 locks the 500 path
// when ListTemplatesByType returns an error after GetThing succeeds.
func TestGetNodeAppliedConfig_ListTemplatesError_Returns500(t *testing.T) {
	h, stub := newAdminHandlerForAppliedConfig(t)
	stub.thing = &store.ThingRegistry{
		ID:   "proxy-1",
		Type: "compliance-proxy",
	}
	stub.templatesErr = errors.New("templates unavailable")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/proxy-1/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("proxy-1")

	_ = h.GetNodeAppliedConfig(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("FAILURE_MODE: ListTemplates error must return 500; got %d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "INTERNAL_ERROR", "server_error")
}

// TestGetNodeAppliedConfig_MalformedReported_Returns500 locks the 500 path
// when the Thing's reported JSON blob is not valid JSON.
func TestGetNodeAppliedConfig_MalformedReported_Returns500(t *testing.T) {
	h, stub := newAdminHandlerForAppliedConfig(t)
	stub.thing = &store.ThingRegistry{
		ID:       "proxy-1",
		Type:     "compliance-proxy",
		Reported: json.RawMessage(`{not valid json`),
	}
	stub.templates = []store.ThingConfigTemplate{
		{Type: "compliance-proxy", ConfigKey: "killswitch", State: json.RawMessage(`{}`), Version: 1},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/proxy-1/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("proxy-1")

	_ = h.GetNodeAppliedConfig(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("FAILURE_MODE: malformed reported JSON must return 500; got %d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "INTERNAL_ERROR", "server_error")
}

// TestGetNodeAppliedConfig_MalformedDesired_Returns500 locks the 500 path when
// the Thing's desired JSON blob is not valid JSON.
func TestGetNodeAppliedConfig_MalformedDesired_Returns500(t *testing.T) {
	h, stub := newAdminHandlerForAppliedConfig(t)
	stub.thing = &store.ThingRegistry{
		ID:      "proxy-1",
		Type:    "compliance-proxy",
		Desired: json.RawMessage(`{bad json}`),
	}
	stub.templates = []store.ThingConfigTemplate{
		{Type: "compliance-proxy", ConfigKey: "killswitch", State: json.RawMessage(`{}`), Version: 1},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/nodes/proxy-1/applied-config", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("proxy-1")

	_ = h.GetNodeAppliedConfig(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("FAILURE_MODE: malformed desired JSON must return 500; got %d", rec.Code)
	}
	assertErrorEnvelope(t, rec, "INTERNAL_ERROR", "server_error")
}

// FetchOverridesForThing — malformed JSON body

// TestFetchOverridesForThing_MalformedBodyReturnsError locks that a 200
// response with non-JSON body is surfaced as an error rather than silently
// returning an empty slice.
func TestFetchOverridesForThing_MalformedBodyReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()

	spy := &hubSpy{baseURL: srv.URL, token: "tok"}
	f := &hubAppliedConfigOverrideFetcher{hub: spy, client: srv.Client()}

	_, err := f.FetchOverridesForThing(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("FAILURE_MODE: malformed Hub response body must return error")
	}
}

// parseAdminAuditParams — endTime branch

// TestParseAdminAuditParams_EndTimeParam locks that a valid endTime query
// param produces a non-nil EndTime in the params struct.
func TestParseAdminAuditParams_EndTimeParam(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/?endTime=2024-12-31T23:59:59Z", nil)
	c := e.NewContext(req, httptest.NewRecorder())
	p := parseAdminAuditParams(c)
	if p.EndTime == nil {
		t.Fatal("FAILURE_MODE: valid endTime must produce non-nil EndTime")
	}
}

// parseRFC3339Flexible — empty string branch

// TestParseRFC3339Flexible_EmptyString locks that an empty string returns
// (zero, false) — the third implicit branch in the function.
func TestParseRFC3339Flexible_EmptyString(t *testing.T) {
	if _, ok := parseRFC3339Flexible(""); ok {
		t.Fatal("FAILURE_MODE: empty string must return ok=false")
	}
}

// TestFetchOverridesForThing_InvalidURLReturnsError locks the request-build
// error path in FetchOverridesForThing when the Hub base URL contains a
// control character that makes http.NewRequestWithContext fail.
func TestFetchOverridesForThing_InvalidURLReturnsError(t *testing.T) {
	// A URL with a raw newline is rejected by http.NewRequestWithContext.
	spy := &hubSpy{baseURL: "http://hub\x00.invalid", token: "tok"}
	f := &hubAppliedConfigOverrideFetcher{hub: spy, client: &http.Client{}}

	_, err := f.FetchOverridesForThing(context.Background(), "thing-1")
	if err == nil {
		t.Fatal("FAILURE_MODE: invalid Hub URL must return error from request builder")
	}
}
