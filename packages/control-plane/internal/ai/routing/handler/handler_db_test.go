package routing

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
)

// routingRuleCols mirrors store/routing_rule.go rrColumns. Kept local so the
// handler tests stay independent of the store-package test helper.
// Column order: id, name, description, strategyType, config, matchConditions,
// priority, pipelineStage, fallbackChain, retryPolicy,
// enabled, createdAt, updatedAt.
var routingRuleCols = []string{
	"id", "name", "description", "strategyType", "config", "matchConditions",
	"priority", "pipelineStage", "fallbackChain", "retryPolicy",
	"enabled", "createdAt", "updatedAt",
}

// makeRRRow returns one 13-column row matching the production scanner. desc is
// passed by value so tests can vary it; retryPolicy is left as SQL NULL by
// default.
func makeRRRow(id, name string, now time.Time) []any {
	desc := "test"
	return []any{
		id, name, &desc, "single",
		json.RawMessage(`{"providerId":"p","modelId":"m"}`),
		json.RawMessage(`{}`),
		0, 1,
		json.RawMessage(`[]`),
		nil, // retryPolicy → NULL
		true,
		now, now,
	}
}

// newHandlerWithMockDB wires a routing.Handler with a pgxmock DB pool, a Hub
// spy, and the audit-spy + silent logger from test_helpers_test.go. Reuses
// hubSpy + auditSpy + silentLogger() already in the package.
func newHandlerWithMockDB(t *testing.T) (*Handler, pgxmock.PgxPoolIface, *hubSpy, *auditSpy) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	hub := &hubSpy{}
	aud := &auditSpy{}
	// Logger that routes to t.Log so DB-error branches surface in -v runs.
	logger := slog.New(slog.NewTextHandler(testLogWriter{t: t}, nil))
	h := New(Deps{
		Pool:   mock,
		Meta:   systemmetastore.New(mock),
		Hub:    hub,
		Audit:  audit.NewWriter(aud, "admin-audit", silentLogger()),
		Logger: logger,
	})
	return h, mock, hub, aud
}

// testLogWriter is an io.Writer that funnels lines into t.Log so the
// production handler's slog Error calls show up in -v test output.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// anyN returns N pgxmock.AnyArg() entries so ExpectQuery.WithArgs can match
// SQL statements with many positional parameters without re-listing them all.
func anyN(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = pgxmock.AnyArg()
	}
	return out
}

// makeJSONReq builds an Echo context wired with a JSON body + admin auth so
// handlers can call audit.EntryFor without panicking.
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

// helper-copies (handler.go)

// TestInternalServerError verifies the helper writes the standard error
// envelope at 500 with type="server_error". This is the only line of the
// helper so the test single-shots the function.
func TestInternalServerError(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/x", nil), rec)
	h := &Handler{}
	_ = h // helper is a free function; receiver-less invocation below

	if err := internalServerError(c, "boom"); err != nil {
		t.Fatalf("internalServerError: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Message != "boom" || body.Error.Type != "server_error" {
		t.Errorf("body = %+v", body)
	}
}

// TestActorFromContext_Present verifies that the helper reads KeyID + KeyName
// from the AdminAuth attached by middleware. The echoContext helper already
// wires WithAdminAuth, so the happy path is exercised by every other DB test
// — this one targets the "actor exists" branch explicitly.
func TestActorFromContext_Present(t *testing.T) {
	c, _ := makeJSONReq(t, http.MethodGet, "/x", "")
	a := actorFromContext(c)
	if a.UserID != "admin-1" || a.Name != "Admin" {
		t.Errorf("actor = %+v; want {UserID:admin-1, Name:Admin}", a)
	}
}

// TestActorFromContext_Absent locks the zero-value fallback when middleware
// has not run. Without this guard a downstream NPE would mask the missing
// auth bug at runtime.
func TestActorFromContext_Absent(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/x", nil), rec)
	a := actorFromContext(c)
	if a.UserID != "" || a.Name != "" {
		t.Errorf("expected zero actor; got %+v", a)
	}
}

// TestParsePagination covers the four caller-visible branches: defaults,
// explicit valid values, the 1000-row clamp, and ignored invalid values.
func TestParsePagination(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
	}{
		{name: "defaults when query empty", query: "", wantLimit: 50, wantOffset: 0},
		{name: "explicit valid", query: "?limit=10&offset=5", wantLimit: 10, wantOffset: 5},
		{name: "limit clamped to 1000", query: "?limit=99999", wantLimit: 1000, wantOffset: 0},
		{name: "negative limit ignored", query: "?limit=-3", wantLimit: 50, wantOffset: 0},
		{name: "zero limit ignored", query: "?limit=0", wantLimit: 50, wantOffset: 0},
		{name: "negative offset ignored", query: "?offset=-1", wantLimit: 50, wantOffset: 0},
		{name: "non-numeric ignored", query: "?limit=abc&offset=xyz", wantLimit: 50, wantOffset: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodGet, "/x"+tc.query, nil), rec)
			pg := parsePagination(c)
			if pg.Limit != tc.wantLimit || pg.Offset != tc.wantOffset {
				t.Errorf("got Limit=%d Offset=%d; want Limit=%d Offset=%d",
					pg.Limit, pg.Offset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

// TestIncrementConfigVersion_GetMissingStartsAtOne covers the "no existing
// row → SET key=1" branch. system_metadata returns ErrNoRows which the helper
// must treat as version-0 (so the increment lands at 1).
func TestIncrementConfigVersion_GetMissingStartsAtOne(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("agent.config.version").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte(`1`), "system").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestIncrementConfigVersion_GetExistingIncrements covers the "row found →
// SET key=existing+1" branch. The Set arg is the JSON-encoded next version.
func TestIncrementConfigVersion_GetExistingIncrements(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`7`)))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte(`8`), "system").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestIncrementConfigVersion_SetFailureLogs covers the warn-log-and-return
// branch when the upsert errors. The helper must NOT panic; failure observed
// only via logger which we route to io.Discard.
func TestIncrementConfigVersion_SetFailureLogs(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("agent.config.version").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte(`1`), "system").
		WillReturnError(errors.New("disk full"))

	h.incrementConfigVersion(context.Background()) // must not panic
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestIncrementConfigVersion_GetGarbageJSON covers the "value column is not
// valid JSON number" branch — the helper silently falls back to version=0.
func TestIncrementConfigVersion_GetGarbageJSON(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`"oops"`)))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte(`1`), "system").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	h.incrementConfigVersion(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestRegisterRoutingRoutes_MountsAllSixVerbs locks the path/verb grid
// against accidental rename/drop. Each entry uses a wildcard ":id" param
// where applicable; the test asserts the exact set of routes Echo would
// expose for the routing domain.
func TestRegisterRoutingRoutes_MountsAllSixVerbs(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	e := echo.New()
	g := e.Group("/api/admin")
	iamMW := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterRoutingRoutes(g, iamMW)

	want := map[string]bool{
		"GET /api/admin/routing-rules":           false,
		"POST /api/admin/routing-rules":          false,
		"POST /api/admin/routing-rules/simulate": false,
		"GET /api/admin/routing-rules/:id":       false,
		"PUT /api/admin/routing-rules/:id":       false,
		"DELETE /api/admin/routing-rules/:id":    false,
	}
	for _, r := range e.Routes() {
		key := r.Method + " " + r.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for k, v := range want {
		if !v {
			t.Errorf("route %q was not registered", k)
		}
	}
}

// TestListRoutingRules_DefaultsHappy covers the most-common admin call: no
// query string, both filter pointers nil, returns the rows + total verbatim.
func TestListRoutingRules_DefaultsHappy(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "RoutingRule"`).
		WithArgs().
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(2))
	mock.ExpectQuery(`FROM "RoutingRule"`).
		WithArgs(50, 0).
		WillReturnRows(pgxmock.NewRows(routingRuleCols).
			AddRow(makeRRRow("rule-1", "first", now)...).
			AddRow(makeRRRow("rule-2", "second", now)...))

	c, rec := makeJSONReq(t, http.MethodGet, "/api/admin/routing-rules", "")
	if err := h.ListRoutingRules(c); err != nil {
		t.Fatalf("ListRoutingRules: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data  []map[string]any `json:"data"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Total != 2 || len(body.Data) != 2 {
		t.Errorf("body = %+v", body)
	}
}

// TestListRoutingRules_AppliesFilters drives the enabled=true + strategyType
// + q query parameters into the store params struct.
func TestListRoutingRules_AppliesFilters(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "RoutingRule"`).
		WithArgs(true, "weighted", "%alpha%").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM "RoutingRule"`).
		WithArgs(true, "weighted", "%alpha%", 50, 0).
		WillReturnRows(pgxmock.NewRows(routingRuleCols))

	c, rec := makeJSONReq(t, http.MethodGet,
		"/api/admin/routing-rules?enabled=true&strategyType=weighted&q=alpha", "")
	if err := h.ListRoutingRules(c); err != nil {
		t.Fatalf("ListRoutingRules: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestListRoutingRules_EnabledFalse pins the enabled=false branch separately
// because the handler builds a distinct *bool the way enabled=true does not.
func TestListRoutingRules_EnabledFalse(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "RoutingRule"`).
		WithArgs(false).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM "RoutingRule"`).
		WithArgs(false, 50, 0).
		WillReturnRows(pgxmock.NewRows(routingRuleCols))

	c, rec := makeJSONReq(t, http.MethodGet, "/api/admin/routing-rules?enabled=false", "")
	if err := h.ListRoutingRules(c); err != nil {
		t.Fatalf("ListRoutingRules: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestListRoutingRules_DBError surfaces the 500 envelope when the count query
// fails.
func TestListRoutingRules_DBError(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "RoutingRule"`).
		WillReturnError(&pgconn.PgError{Code: "08006", Message: "conn down"})

	c, rec := makeJSONReq(t, http.MethodGet, "/api/admin/routing-rules", "")
	if err := h.ListRoutingRules(c); err != nil {
		t.Fatalf("ListRoutingRules: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "server_error")
}

func TestGetRoutingRule_Happy(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "abc", now)...))

	c, rec := makeJSONReq(t, http.MethodGet, "/api/admin/routing-rules/rule-1", "")
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.GetRoutingRule(c); err != nil {
		t.Fatalf("GetRoutingRule: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":"rule-1"`) {
		t.Errorf("body missing id; got %s", rec.Body.String())
	}
}

func TestGetRoutingRule_NotFound(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule"`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	c, rec := makeJSONReq(t, http.MethodGet, "/api/admin/routing-rules/missing", "")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.GetRoutingRule(c); err != nil {
		t.Fatalf("GetRoutingRule: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "", "not_found")
}

func TestGetRoutingRule_DBError(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule"`).
		WithArgs("rule-1").
		WillReturnError(errors.New("boom"))

	c, rec := makeJSONReq(t, http.MethodGet, "/api/admin/routing-rules/rule-1", "")
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.GetRoutingRule(c); err != nil {
		t.Fatalf("GetRoutingRule: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
}

// TestCreateRoutingRule_HappyPath drives the full happy path including
// audit-log emission and Hub invalidation.
func TestCreateRoutingRule_HappyPath(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`INSERT INTO "RoutingRule"`).
		WithArgs(anyN(10)...).
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-new", "n", now)...))

	body := `{
		"name":"n","strategyType":"single","config":{"providerId":"p","modelId":"m"}
	}`
	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/routing-rules", body)
	if err := h.CreateRoutingRule(c); err != nil {
		t.Fatalf("CreateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(hub.invalidateCalls) != 1 {
		t.Errorf("expected 1 hub invalidate; got %d", len(hub.invalidateCalls))
	}
	if hub.invalidateCalls[0].ThingType != "ai-gateway" || hub.invalidateCalls[0].ConfigKey != "routing_rules" {
		t.Errorf("hub call args wrong: %+v", hub.invalidateCalls[0])
	}
	if aud.count() != 1 {
		t.Errorf("expected 1 audit entry; got %d", aud.count())
	}
}

// TestCreateRoutingRule_PipelineStageZero pins the explicit-0 stage path
// (handler defaults to 1 when nil, must accept 0 explicitly).
func TestCreateRoutingRule_PipelineStageZero(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`INSERT INTO "RoutingRule"`).
		WithArgs(anyN(10)...).
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-x", "n", now)...))

	body := `{
		"name":"n","strategyType":"single","config":{"providerId":"p","modelId":"m"},
		"pipelineStage":0,"enabled":false
	}`
	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/routing-rules", body)
	if err := h.CreateRoutingRule(c); err != nil {
		t.Fatalf("CreateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// TestCreateRoutingRule_BindError covers the c.Bind failure path: non-JSON
// body returns 400 with validation_error.
func TestCreateRoutingRule_BindError(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/routing-rules", `{bogus`)
	if err := h.CreateRoutingRule(c); err != nil {
		t.Fatalf("CreateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "validation_error")
}

// TestCreateRoutingRule_MissingRequired covers the post-bind validation that
// rejects empty name / strategyType / config.
func TestCreateRoutingRule_MissingRequired(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "empty name", body: `{"name":"","strategyType":"single","config":{}}`},
		{name: "empty strategy", body: `{"name":"n","strategyType":"","config":{}}`},
		{name: "no config", body: `{"name":"n","strategyType":"single"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, _ := newHandlerWithMockDB(t)
			c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/routing-rules", tc.body)
			if err := h.CreateRoutingRule(c); err != nil {
				t.Fatalf("CreateRoutingRule: %v", err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400; body=%s", rec.Code, rec.Body.String())
			}
			assertErrorEnvelope(t, rec, "", "validation_error")
		})
	}
}

// TestCreateRoutingRule_LegacyMatchConditionsRejected proves the rename
// guard short-circuits with 422.
func TestCreateRoutingRule_LegacyMatchConditionsRejected(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	body := `{"name":"n","strategyType":"single","config":{"a":1},"matchConditions":{"organizations":["p"]}}`
	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/routing-rules", body)
	if err := h.CreateRoutingRule(c); err != nil {
		t.Fatalf("CreateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d; want 422", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "match_conditions_legacy_field")
}

// TestCreateRoutingRule_SmartRuleUnsafe proves the smart-rule match-condition guard fires.
func TestCreateRoutingRule_SmartRuleUnsafe(t *testing.T) {
	h, _, _, _ := newHandlerWithMockDB(t)
	body := `{"name":"n","strategyType":"smart","config":{"a":1},"matchConditions":{}}`
	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/routing-rules", body)
	if err := h.CreateRoutingRule(c); err != nil {
		t.Fatalf("CreateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "smart_rule_match_conditions_unsafe")
}

// TestCreateRoutingRule_DBFails covers the 500 envelope when the INSERT
// fails. The handler must still emit no audit entry and no Hub invalidate
// when the DB writes fail.
func TestCreateRoutingRule_DBFails(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	mock.ExpectQuery(`INSERT INTO "RoutingRule"`).
		WithArgs(anyN(10)...).
		WillReturnError(errors.New("constraint violation"))

	body := `{"name":"n","strategyType":"single","config":{"a":1}}`
	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/routing-rules", body)
	if err := h.CreateRoutingRule(c); err != nil {
		t.Fatalf("CreateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
	if len(hub.invalidateCalls) != 0 || aud.count() != 0 {
		t.Errorf("expected no side effects on DB failure")
	}
}

// TestCreateRoutingRule_NoHub exercises the nil-hub guard branch.
func TestCreateRoutingRule_NoHub(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(mock.Close)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`INSERT INTO "RoutingRule"`).
		WithArgs(anyN(10)...).
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("r", "n", now)...))

	aud := &auditSpy{}
	h := New(Deps{
		Pool:   mock,
		Meta:   systemmetastore.New(mock),
		Hub:    nil, // explicit nil
		Audit:  audit.NewWriter(aud, "audit", silentLogger()),
		Logger: silentLogger(),
	})
	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/routing-rules",
		`{"name":"n","strategyType":"single","config":{"a":1}}`)
	if err := h.CreateRoutingRule(c); err != nil {
		t.Fatalf("CreateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201", rec.Code)
	}
}

// TestUpdateRoutingRule_NotFound covers the early-return 404 path when the
// existing row lookup returns nil.
func TestUpdateRoutingRule_NotFound(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	c, rec := makeJSONReq(t, http.MethodPut, "/api/admin/routing-rules/missing", `{"name":"new"}`)
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.UpdateRoutingRule(c); err != nil {
		t.Fatalf("UpdateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "not_found")
}

// TestUpdateRoutingRule_ExistingLookupErrors covers the 500 path when the
// initial GetRoutingRule itself returns a non-ErrNoRows error.
func TestUpdateRoutingRule_ExistingLookupErrors(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnError(errors.New("conn closed"))

	c, rec := makeJSONReq(t, http.MethodPut, "/api/admin/routing-rules/rule-1", `{"name":"n"}`)
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.UpdateRoutingRule(c); err != nil {
		t.Fatalf("UpdateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
}

// TestUpdateRoutingRule_Happy exercises the full happy path including all
// non-nil pointer parameter branches (name + strategy + priority + stage +
// enabled + config + matchConditions + fallbackChain).
func TestUpdateRoutingRule_Happy(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "old", now)...))
	mock.ExpectQuery(`UPDATE "RoutingRule"`).
		WithArgs(anyN(12)...).
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "new", now)...))

	body := `{
		"name":"new","description":"d","strategyType":"single",
		"config":{"providerId":"p2"},
		"matchConditions":{"projects":["px"]},
		"priority":7,"pipelineStage":1,
		"fallbackChain":[],
		"enabled":true,
		"retryPolicy":{"maxAttemptsPerTarget":3}
	}`
	c, rec := makeJSONReq(t, http.MethodPut, "/api/admin/routing-rules/rule-1", body)
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.UpdateRoutingRule(c); err != nil {
		t.Fatalf("UpdateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(hub.invalidateCalls) != 1 || aud.count() != 1 {
		t.Errorf("expected 1 hub invalidate + 1 audit; got hub=%d audit=%d",
			len(hub.invalidateCalls), aud.count())
	}
}

// TestUpdateRoutingRule_PipelineStageZero pins the explicit-0 stage path on
// update (handler defaults to 1 when *int is non-nil but zero; explicit 0
// must be respected).
func TestUpdateRoutingRule_PipelineStageZero(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "old", now)...))
	mock.ExpectQuery(`UPDATE "RoutingRule"`).
		WithArgs(anyN(12)...).
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "old", now)...))

	c, rec := makeJSONReq(t, http.MethodPut, "/api/admin/routing-rules/rule-1", `{"pipelineStage":0}`)
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.UpdateRoutingRule(c); err != nil {
		t.Fatalf("UpdateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
}

// TestUpdateRoutingRule_RetryPolicyInvalid pins the 400 path when the
// retryPolicy field is present but fails validation.
func TestUpdateRoutingRule_RetryPolicyInvalid(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "old", now)...))

	body := `{"retryPolicy":{"maxAttemptsPerTarget":99}}`
	c, rec := makeJSONReq(t, http.MethodPut, "/api/admin/routing-rules/rule-1", body)
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.UpdateRoutingRule(c); err != nil {
		t.Fatalf("UpdateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "retry_policy_invalid")
}

// TestUpdateRoutingRule_LegacyMatchConditions covers the 422 envelope on the
// update path's match-conditions guard.
func TestUpdateRoutingRule_LegacyMatchConditions(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "old", now)...))

	body := `{"matchConditions":{"organizations":["o"]}}`
	c, rec := makeJSONReq(t, http.MethodPut, "/api/admin/routing-rules/rule-1", body)
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.UpdateRoutingRule(c); err != nil {
		t.Fatalf("UpdateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d; want 422", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "match_conditions_legacy_field")
}

// TestUpdateRoutingRule_SmartRuleGuardFires covers the case where the
// update body carries both strategyType=smart and unsafe matchConditions.
func TestUpdateRoutingRule_SmartRuleGuardFires(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "old", now)...))

	// strategyType + matchConditions both present → smart guard runs.
	body := `{"strategyType":"smart","matchConditions":{"projects":["p"]}}`
	c, rec := makeJSONReq(t, http.MethodPut, "/api/admin/routing-rules/rule-1", body)
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.UpdateRoutingRule(c); err != nil {
		t.Fatalf("UpdateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "smart_rule_match_conditions_unsafe")
}

// TestUpdateRoutingRule_BindError exercises the bind failure (e.g., non-JSON
// body). Note: the handler reads rawBody before Bind, so an unreadable body
// path is not reachable via httptest — only malformed JSON.
func TestUpdateRoutingRule_BindError(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "old", now)...))

	c, rec := makeJSONReq(t, http.MethodPut, "/api/admin/routing-rules/rule-1", `{not-json`)
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.UpdateRoutingRule(c); err != nil {
		t.Fatalf("UpdateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "validation_error")
}

// TestUpdateRoutingRule_DBUpdateFails exercises the 500 envelope when the
// UPDATE itself fails.
func TestUpdateRoutingRule_DBUpdateFails(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "old", now)...))
	mock.ExpectQuery(`UPDATE "RoutingRule"`).WithArgs(anyN(12)...).WillReturnError(errors.New("update boom"))

	c, rec := makeJSONReq(t, http.MethodPut, "/api/admin/routing-rules/rule-1", `{"name":"x"}`)
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.UpdateRoutingRule(c); err != nil {
		t.Fatalf("UpdateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
	if len(hub.invalidateCalls) != 0 || aud.count() != 0 {
		t.Errorf("expected no side effects on UPDATE failure")
	}
}

// TestUpdateRoutingRule_NoHubAllowed covers nil Hub on the update path.
func TestUpdateRoutingRule_NoHubAllowed(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(mock.Close)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "old", now)...))
	mock.ExpectQuery(`UPDATE "RoutingRule"`).
		WithArgs(anyN(12)...).
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "new", now)...))

	aud := &auditSpy{}
	h := New(Deps{
		Pool:   mock,
		Meta:   systemmetastore.New(mock),
		Hub:    nil,
		Audit:  audit.NewWriter(aud, "audit", silentLogger()),
		Logger: silentLogger(),
	})
	c, rec := makeJSONReq(t, http.MethodPut, "/api/admin/routing-rules/rule-1", `{"name":"y"}`)
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.UpdateRoutingRule(c); err != nil {
		t.Fatalf("UpdateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
}

func TestDeleteRoutingRule_Happy(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "old", now)...))
	mock.ExpectExec(`DELETE FROM "RoutingRule"`).
		WithArgs("rule-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	c, rec := makeJSONReq(t, http.MethodDelete, "/api/admin/routing-rules/rule-1", "")
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.DeleteRoutingRule(c); err != nil {
		t.Fatalf("DeleteRoutingRule: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d; want 204", rec.Code)
	}
	if len(hub.invalidateCalls) != 1 || aud.count() != 1 {
		t.Errorf("expected 1 hub invalidate + 1 audit; got hub=%d audit=%d",
			len(hub.invalidateCalls), aud.count())
	}
}

func TestDeleteRoutingRule_NotFound(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	c, rec := makeJSONReq(t, http.MethodDelete, "/api/admin/routing-rules/missing", "")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	if err := h.DeleteRoutingRule(c); err != nil {
		t.Fatalf("DeleteRoutingRule: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "not_found")
}

func TestDeleteRoutingRule_GetExistingErrors(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnError(errors.New("conn lost"))

	c, rec := makeJSONReq(t, http.MethodDelete, "/api/admin/routing-rules/rule-1", "")
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.DeleteRoutingRule(c); err != nil {
		t.Fatalf("DeleteRoutingRule: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
}

func TestDeleteRoutingRule_DBDeleteFails(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "old", now)...))
	mock.ExpectExec(`DELETE FROM "RoutingRule"`).
		WithArgs("rule-1").
		WillReturnError(errors.New("delete boom"))

	c, rec := makeJSONReq(t, http.MethodDelete, "/api/admin/routing-rules/rule-1", "")
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.DeleteRoutingRule(c); err != nil {
		t.Fatalf("DeleteRoutingRule: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
	if len(hub.invalidateCalls) != 0 || aud.count() != 0 {
		t.Errorf("expected no side effects on DELETE failure")
	}
}

// Edge branches that the existing validate/simulate tests do not cover

// TestValidateMatchConditions_MalformedJSON exercises the json.Unmarshal
// failure branch — the validator returns ("", true) so the caller's
// bind-time validation takes over.
func TestValidateMatchConditions_MalformedJSON(t *testing.T) {
	msg, ok := validateMatchConditions(json.RawMessage(`{not-json`))
	if !ok || msg != "" {
		t.Errorf("expected ok=true msg='' on malformed JSON; got ok=%v msg=%q", ok, msg)
	}
}

// TestValidateSmartRuleMatchConditions_MalformedJSON exercises the json
// fall-through; smart-strategy with malformed JSON also defers to bind.
func TestValidateSmartRuleMatchConditions_MalformedJSON(t *testing.T) {
	msg, ok := validateSmartRuleMatchConditions("smart", json.RawMessage(`{not-json`))
	if !ok || msg != "" {
		t.Errorf("expected ok=true msg='' on malformed JSON; got ok=%v msg=%q", ok, msg)
	}
}

// TestValidateSmartRuleMatchConditions_LiteralsNotArray covers the branch
// where requestedModelLiterals is present but unmarshals to a non-array
// (e.g. an object). The validator defers to bind-time validation.
func TestValidateSmartRuleMatchConditions_LiteralsNotArray(t *testing.T) {
	msg, ok := validateSmartRuleMatchConditions("smart",
		json.RawMessage(`{"requestedModelLiterals":{"oops":"object"}}`))
	if !ok || msg != "" {
		t.Errorf("expected ok=true msg='' when literals is not an array; got ok=%v msg=%q", ok, msg)
	}
}

// TestRoutingSimulate_BindError covers the bad-JSON branch — Echo's default
// Binder rejects a malformed body with an error that the handler surfaces as
// 400/validation_error.
func TestRoutingSimulate_BindError(t *testing.T) {
	h := New(Deps{
		Proxy:  ProxyConfig{AIGatewayURL: "http://127.0.0.1:1"},
		Logger: silentLogger(),
	})
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/routing-rules/simulate",
		bytes.NewReader([]byte(`{not-json`)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.RoutingSimulate(c); err != nil {
		t.Fatalf("RoutingSimulate: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
	assertErrorEnvelope(t, rec, "", "validation_error")
}

// TestUpdateRoutingRule_BodyReadFailure exercises the io.ReadAll error branch
// by attaching a request body whose Read always errors. The handler must
// surface this as 400/validation_error rather than panicking.
func TestUpdateRoutingRule_BodyReadFailure(t *testing.T) {
	h, mock, _, _ := newHandlerWithMockDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "old", now)...))

	req := httptest.NewRequest(http.MethodPut, "/api/admin/routing-rules/rule-1", failingReader{})
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "Admin", "admin-1")
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.UpdateRoutingRule(c); err != nil {
		t.Fatalf("UpdateRoutingRule: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "", "validation_error")
}

// failingReader implements io.Reader by always returning an error, used to
// drive the io.ReadAll failure branch in UpdateRoutingRule.
type failingReader struct{}

func (failingReader) Read(_ []byte) (int, error) { return 0, errors.New("simulated read error") }

func TestDeleteRoutingRule_NoHubAllowed(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(mock.Close)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "old", now)...))
	mock.ExpectExec(`DELETE FROM "RoutingRule"`).
		WithArgs("rule-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	aud := &auditSpy{}
	h := New(Deps{
		Pool:   mock,
		Meta:   systemmetastore.New(mock),
		Hub:    nil,
		Audit:  audit.NewWriter(aud, "audit", silentLogger()),
		Logger: silentLogger(),
	})
	c, rec := makeJSONReq(t, http.MethodDelete, "/api/admin/routing-rules/rule-1", "")
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.DeleteRoutingRule(c); err != nil {
		t.Fatalf("DeleteRoutingRule: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d; want 204", rec.Code)
	}
}
