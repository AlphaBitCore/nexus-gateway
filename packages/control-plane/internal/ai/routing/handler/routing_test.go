package routing

import (
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
)

// TestValidateRetryPolicyJSON exercises the per-rule policy bounds checker
// independently of the HTTP handler. Covers the spec §6.2 enums and the
// [1,5] clamp on maxAttemptsPerTarget.
func TestValidateRetryPolicyJSON(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantOK  bool
		wantSub string // substring of error message; empty when wantOK
	}{
		{name: "empty raw is allowed (means clear / inherit default)", raw: "", wantOK: true},
		{name: "literal null is allowed", raw: "null", wantOK: true},
		{name: "fully populated valid policy", raw: `{"maxAttemptsPerTarget":3,"retryOn":["5xx","timeout"]}`, wantOK: true},
		{name: "empty object is allowed (inherits all defaults)", raw: `{}`, wantOK: true},
		{name: "empty retryOn array is allowed (means 'retry nothing')", raw: `{"retryOn":[]}`, wantOK: true},
		{name: "max attempts at lower bound 1", raw: `{"maxAttemptsPerTarget":1}`, wantOK: true},
		{name: "max attempts at upper bound 5", raw: `{"maxAttemptsPerTarget":5}`, wantOK: true},
		{name: "max attempts above ceiling 6", raw: `{"maxAttemptsPerTarget":6}`, wantOK: false, wantSub: "must be in [1,5]"},
		{name: "max attempts negative", raw: `{"maxAttemptsPerTarget":-1}`, wantOK: false, wantSub: "must be in [1,5]"},
		// max attempts == 0 means "absent" (struct zero), and is allowed because
		// MergedWith treats 0 as "fall back to default".
		{name: "max attempts zero (absent sentinel)", raw: `{"maxAttemptsPerTarget":0}`, wantOK: true},
		{name: "retryOn bogus class", raw: `{"retryOn":["bogus"]}`, wantOK: false, wantSub: "is not a valid error class"},
		{name: "retryOn mixed valid + invalid", raw: `{"retryOn":["5xx","oops"]}`, wantOK: false, wantSub: "is not a valid error class"},
		{name: "retryOn all four valid", raw: `{"retryOn":["network","timeout","429","5xx"]}`, wantOK: true},
		{name: "malformed JSON", raw: `{not json`, wantOK: false, wantSub: "not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.raw != "" {
				raw = json.RawMessage(tc.raw)
			}
			msg, ok := validateRetryPolicyJSON(raw)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (msg=%q)", ok, tc.wantOK, msg)
			}
			if !tc.wantOK && tc.wantSub != "" && !contains(msg, tc.wantSub) {
				t.Errorf("msg = %q; want substring %q", msg, tc.wantSub)
			}
		})
	}
}

// TestExtractRetryPolicyForUpdate exercises the absent-vs-null distinction
// that PUT /routing-rules/{id} needs to translate into store-layer intent.
func TestExtractRetryPolicyForUpdate(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantPresent bool
		wantRawIs   string // expected string form of the returned raw; "" when raw should be empty
	}{
		{name: "field absent", body: `{"name":"r1"}`, wantPresent: false, wantRawIs: ""},
		{name: "field is JSON null", body: `{"retryPolicy":null}`, wantPresent: true, wantRawIs: ""},
		{name: "field is empty object", body: `{"retryPolicy":{}}`, wantPresent: true, wantRawIs: "{}"},
		{name: "field has policy", body: `{"retryPolicy":{"maxAttemptsPerTarget":3}}`, wantPresent: true, wantRawIs: `{"maxAttemptsPerTarget":3}`},
		{name: "empty body", body: ``, wantPresent: false, wantRawIs: ""},
		{name: "non-object body", body: `"a string"`, wantPresent: false, wantRawIs: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, present, errMsg := extractRetryPolicyForUpdate([]byte(tc.body))
			if errMsg != "" {
				t.Fatalf("unexpected errMsg = %q", errMsg)
			}
			if present != tc.wantPresent {
				t.Fatalf("present = %v, want %v", present, tc.wantPresent)
			}
			if string(raw) != tc.wantRawIs {
				t.Errorf("raw = %q, want %q", string(raw), tc.wantRawIs)
			}
		})
	}
}

// TestCreateRoutingRule_ValidatesRetryPolicy_NoDB ensures the handler short-circuits
// invalid retryPolicy payloads with HTTP 400 *before* hitting the DB. We can
// rely on the DB being nil because the validator is the first thing the
// handler does after Bind for retryPolicy errors.
func TestCreateRoutingRule_ValidatesRetryPolicy_NoDB(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "max attempts above ceiling", body: `{"name":"r","strategyType":"single","config":{},"retryPolicy":{"maxAttemptsPerTarget":6}}`},
		{name: "negative max attempts", body: `{"name":"r","strategyType":"single","config":{},"retryPolicy":{"maxAttemptsPerTarget":-1}}`},
		{name: "bogus retryOn class", body: `{"name":"r","strategyType":"single","config":{},"retryPolicy":{"retryOn":["bogus"]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newAdminHandlerWithHubSpy(t)
			req := httptest.NewRequest(http.MethodPost, "/api/admin/routing-rules", bytes.NewReader([]byte(tc.body)))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			c := echoContext(req, rec, "Admin", "admin-1")

			if err := h.CreateRoutingRule(c); err != nil {
				t.Fatalf("CreateRoutingRule: %v", err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			// errJSON puts the snake_case identifier in the "type" slot;
			// "code" stays empty for these validation errors per the
			// convention used by sibling handlers (validateMatchConditions etc).
			assertErrorEnvelope(t, rec, "", "retry_policy_invalid")
		})
	}
}

// TestCreateRoutingRule_RejectsUnknownStrategyType_NoDB ensures the handler
// short-circuits an unrecognized strategyType with HTTP 400 before the DB
// write + fleet-wide InvalidateConfig broadcast (F-0272b). The validator runs
// after Bind and before any store call, so a nil DB never gets reached.
func TestCreateRoutingRule_RejectsUnknownStrategyType_NoDB(t *testing.T) {
	for _, st := range []string{"round-robin", "weighted", "Smart", "bogus"} {
		t.Run(st, func(t *testing.T) {
			h, hub, _ := newAdminHandlerWithHubSpy(t)
			body := `{"name":"r","strategyType":"` + st + `","config":{"type":"single"}}`
			rec := callCreate(t, h, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			assertErrorEnvelope(t, rec, "", "strategy_type_invalid")
			if len(hub.invalidateCalls) != 0 {
				t.Errorf("hub.InvalidateConfig called %d times; want 0 (rejected before broadcast)", len(hub.invalidateCalls))
			}
		})
	}
}

// TestCreateRoutingRule_RejectsMalformedConfig_NoDB ensures a config payload
// that cannot parse as a strategy object is rejected with HTTP 400 before the
// DB write + broadcast (F-0272b).
func TestCreateRoutingRule_RejectsMalformedConfig_NoDB(t *testing.T) {
	cases := []struct{ name, config string }{
		{"json array", `[{"type":"single"}]`},
		{"json string", `"single"`},
		{"wrong field type", `{"type":"single","providerId":123}`},
		{"unknown node type", `{"type":"frobnicate"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, hub, _ := newAdminHandlerWithHubSpy(t)
			body := `{"name":"r","strategyType":"single","config":` + tc.config + `}`
			rec := callCreate(t, h, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			assertErrorEnvelope(t, rec, "", "strategy_config_invalid")
			if len(hub.invalidateCalls) != 0 {
				t.Errorf("hub.InvalidateConfig called %d times; want 0 (rejected before broadcast)", len(hub.invalidateCalls))
			}
		})
	}
}

// TestUpdateRoutingRule_RejectsUnknownStrategyType_NoDB ensures the update
// path rejects an unknown strategyType with HTTP 400. The existing-rule
// lookup happens first, so this needs the DB-backed harness; the test is
// skipped without DATABASE_URL like its sibling integration tests.
func TestUpdateRoutingRule_RejectsUnknownStrategyType_NoDB(t *testing.T) {
	db := openRoutingRuleHandlerTestDB(t)
	h, _, _ := newRoutingHandlerForIT(t, db)

	name := uniqueName(t, "rule-st-update")
	createRec := callCreate(t, h, `{"name":"`+name+`","strategyType":"single","config":{"type":"single","providerId":"p","modelId":"m"}}`)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("seed create status = %d; body=%s", createRec.Code, createRec.Body.String())
	}
	id := decodeIDAndRegisterCleanup(t, db, createRec)

	rec := callUpdate(t, h, id, `{"strategyType":"round-robin"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "", "strategy_type_invalid")
}

// TestUpdateRoutingRule_RejectsMalformedConfig_NoDB ensures the update path
// rejects a malformed config with HTTP 400 (F-0272b).
func TestUpdateRoutingRule_RejectsMalformedConfig_NoDB(t *testing.T) {
	db := openRoutingRuleHandlerTestDB(t)
	h, _, _ := newRoutingHandlerForIT(t, db)

	name := uniqueName(t, "rule-cfg-update")
	createRec := callCreate(t, h, `{"name":"`+name+`","strategyType":"single","config":{"type":"single","providerId":"p","modelId":"m"}}`)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("seed create status = %d; body=%s", createRec.Code, createRec.Body.String())
	}
	id := decodeIDAndRegisterCleanup(t, db, createRec)

	rec := callUpdate(t, h, id, `{"config":["not","an","object"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	assertErrorEnvelope(t, rec, "", "strategy_config_invalid")
}

// TestCreateRoutingRule_AcceptsValidStrategyAndConfig is the positive case:
// a known strategyType with a well-shaped config persists (F-0272b).
func TestCreateRoutingRule_AcceptsValidStrategyAndConfig(t *testing.T) {
	db := openRoutingRuleHandlerTestDB(t)
	h, _, _ := newRoutingHandlerForIT(t, db)

	name := uniqueName(t, "rule-valid-create")
	body := `{"name":"` + name + `","strategyType":"single","config":{"type":"single","providerId":"p","modelId":"m"}}`
	rec := callCreate(t, h, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	decodeIDAndRegisterCleanup(t, db, rec)
}

// DB-backed integration tests (require DATABASE_URL)

func openRoutingRuleHandlerTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping routing rule handler integration test")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect to DB: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newRoutingHandlerForIT wires a real DB with the in-memory hub + audit
// spies, matching the convention used by the interception-domain
// integration test. Returns the handler so each test can drive Create/Update
// directly via Echo contexts.
func newRoutingHandlerForIT(t *testing.T, pool *pgxpool.Pool) (*Handler, *hubSpy, *auditSpy) {
	t.Helper()
	hub := &hubSpy{}
	aud := &auditSpy{}
	h := New(Deps{
		Pool:   pool,
		Meta:   systemmetastore.New(pool),
		Hub:    hub,
		Audit:  audit.NewWriter(aud, "audit", silentLogger()),
		Logger: silentLogger(),
	})
	return h, hub, aud
}

// callCreate runs CreateRoutingRule with the given JSON body and returns the
// recorder so callers can inspect status + body.
func callCreate(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/routing-rules", bytes.NewReader([]byte(body)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "Admin", "admin-1")
	if err := h.CreateRoutingRule(c); err != nil {
		t.Fatalf("CreateRoutingRule: %v", err)
	}
	return rec
}

// callUpdate runs UpdateRoutingRule with the given JSON body for the given id.
func callUpdate(t *testing.T, h *Handler, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/admin/routing-rules/"+id, bytes.NewReader([]byte(body)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "Admin", "admin-1")
	c.SetParamNames("id")
	c.SetParamValues(id)
	if err := h.UpdateRoutingRule(c); err != nil {
		t.Fatalf("UpdateRoutingRule: %v", err)
	}
	return rec
}

// callGet runs GetRoutingRule for the given id.
func callGet(t *testing.T, h *Handler, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/routing-rules/"+id, nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "Admin", "admin-1")
	c.SetParamNames("id")
	c.SetParamValues(id)
	if err := h.GetRoutingRule(c); err != nil {
		t.Fatalf("GetRoutingRule: %v", err)
	}
	return rec
}

// decodeID extracts the rule id from a Create response, registering a cleanup
// to remove the row at test end.
func decodeIDAndRegisterCleanup(t *testing.T, pool *pgxpool.Pool, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode create response: %v; body=%s", err, rec.Body.String())
	}
	id, _ := got["id"].(string)
	if id == "" {
		t.Fatalf("expected non-empty id; body=%s", rec.Body.String())
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "RoutingRule" WHERE id = $1`, id)
	})
	return id
}

// uniqueName makes rule names per-test-unique so reruns + parallel CI don't
// collide on the unique-name constraint. Uses the test name for traceability
// and the wall-clock nanosecond for collision avoidance across reruns.
func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s-%s-%d", prefix, t.Name(), time.Now().UnixNano())
}

func TestCreateRoutingRule_AcceptsRetryPolicy(t *testing.T) {
	db := openRoutingRuleHandlerTestDB(t)
	h, _, _ := newRoutingHandlerForIT(t, db)

	name := uniqueName(t, "rule-rp-create")
	body := `{
		"name":"` + name + `",
		"strategyType":"single",
		"config":{"providerId":"p","modelId":"m"},
		"retryPolicy":{"maxAttemptsPerTarget":3,"retryOn":["5xx","timeout"]}
	}`
	rec := callCreate(t, h, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d; want 201; body=%s", rec.Code, rec.Body.String())
	}
	id := decodeIDAndRegisterCleanup(t, db, rec)

	getRec := callGet(t, h, id)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d; want 200; body=%s", getRec.Code, getRec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	rp, ok := got["retryPolicy"].(map[string]any)
	if !ok {
		t.Fatalf("expected retryPolicy object in GET response; got %v", got["retryPolicy"])
	}
	if v, _ := rp["maxAttemptsPerTarget"].(float64); int(v) != 3 {
		t.Errorf("maxAttemptsPerTarget = %v, want 3", rp["maxAttemptsPerTarget"])
	}
	classes, _ := rp["retryOn"].([]any)
	if len(classes) != 2 {
		t.Errorf("retryOn length = %d, want 2", len(classes))
	}
}

func TestCreateRoutingRule_AcceptsNullRetryPolicy(t *testing.T) {
	db := openRoutingRuleHandlerTestDB(t)
	h, _, _ := newRoutingHandlerForIT(t, db)

	name := uniqueName(t, "rule-rp-null")
	body := `{
		"name":"` + name + `",
		"strategyType":"single",
		"config":{"providerId":"p","modelId":"m"}
	}`
	rec := callCreate(t, h, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d; want 201; body=%s", rec.Code, rec.Body.String())
	}
	id := decodeIDAndRegisterCleanup(t, db, rec)

	getRec := callGet(t, h, id)
	var got map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if rp, present := got["retryPolicy"]; present && rp != nil {
		t.Errorf("expected retryPolicy to be absent or null; got %v", rp)
	}
}

func TestPatchRoutingRule_UpdatesRetryPolicy(t *testing.T) {
	db := openRoutingRuleHandlerTestDB(t)
	h, _, _ := newRoutingHandlerForIT(t, db)

	name := uniqueName(t, "rule-rp-patch-set")
	createBody := `{
		"name":"` + name + `",
		"strategyType":"single",
		"config":{"providerId":"p","modelId":"m"}
	}`
	createRec := callCreate(t, h, createBody)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", createRec.Code, createRec.Body.String())
	}
	id := decodeIDAndRegisterCleanup(t, db, createRec)

	updateRec := callUpdate(t, h, id, `{"retryPolicy":{"maxAttemptsPerTarget":2,"retryOn":["429"]}}`)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update: %d body=%s", updateRec.Code, updateRec.Body.String())
	}

	getRec := callGet(t, h, id)
	var got map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	rp, ok := got["retryPolicy"].(map[string]any)
	if !ok {
		t.Fatalf("expected retryPolicy object after PUT; got %v", got["retryPolicy"])
	}
	if v, _ := rp["maxAttemptsPerTarget"].(float64); int(v) != 2 {
		t.Errorf("maxAttemptsPerTarget = %v, want 2", rp["maxAttemptsPerTarget"])
	}
}

func TestPatchRoutingRule_ClearsRetryPolicy(t *testing.T) {
	db := openRoutingRuleHandlerTestDB(t)
	h, _, _ := newRoutingHandlerForIT(t, db)

	name := uniqueName(t, "rule-rp-patch-clear")
	createBody := `{
		"name":"` + name + `",
		"strategyType":"single",
		"config":{"providerId":"p","modelId":"m"},
		"retryPolicy":{"maxAttemptsPerTarget":3,"retryOn":["5xx"]}
	}`
	createRec := callCreate(t, h, createBody)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", createRec.Code, createRec.Body.String())
	}
	id := decodeIDAndRegisterCleanup(t, db, createRec)

	updateRec := callUpdate(t, h, id, `{"retryPolicy":null}`)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update: %d body=%s", updateRec.Code, updateRec.Body.String())
	}

	getRec := callGet(t, h, id)
	var got map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if rp, present := got["retryPolicy"]; present && rp != nil {
		t.Errorf("expected retryPolicy cleared after PUT null; got %v", rp)
	}
}

func TestPatchRoutingRule_AbsentFieldLeavesPolicyUnchanged(t *testing.T) {
	db := openRoutingRuleHandlerTestDB(t)
	h, _, _ := newRoutingHandlerForIT(t, db)

	name := uniqueName(t, "rule-rp-patch-absent")
	createBody := `{
		"name":"` + name + `",
		"strategyType":"single",
		"config":{"providerId":"p","modelId":"m"},
		"retryPolicy":{"maxAttemptsPerTarget":4,"retryOn":["5xx"]}
	}`
	createRec := callCreate(t, h, createBody)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", createRec.Code, createRec.Body.String())
	}
	id := decodeIDAndRegisterCleanup(t, db, createRec)

	// PUT without retryPolicy must leave the column unchanged.
	updateRec := callUpdate(t, h, id, `{"priority":7}`)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update: %d body=%s", updateRec.Code, updateRec.Body.String())
	}

	getRec := callGet(t, h, id)
	var got map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	rp, ok := got["retryPolicy"].(map[string]any)
	if !ok {
		t.Fatalf("expected retryPolicy preserved when field absent; got %v", got["retryPolicy"])
	}
	if v, _ := rp["maxAttemptsPerTarget"].(float64); int(v) != 4 {
		t.Errorf("maxAttemptsPerTarget = %v, want 4 (preserved)", rp["maxAttemptsPerTarget"])
	}
}

func TestCreateRoutingRule_ValidatesMaxAttemptsRange_DB(t *testing.T) {
	// Same intent as the no-DB test but exercises the full handler against
	// a real DB to confirm we don't accidentally write a bad row.
	db := openRoutingRuleHandlerTestDB(t)
	h, _, _ := newRoutingHandlerForIT(t, db)

	name := uniqueName(t, "rule-rp-attempts")
	body := `{
		"name":"` + name + `",
		"strategyType":"single",
		"config":{"providerId":"p","modelId":"m"},
		"retryPolicy":{"maxAttemptsPerTarget":42}
	}`
	rec := callCreate(t, h, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400; body=%s", rec.Code, rec.Body.String())
	}
	// Belt-and-braces: confirm no row landed.
	var n int
	_ = db.QueryRow(context.Background(), `SELECT COUNT(*) FROM "RoutingRule" WHERE name = $1`, name).Scan(&n)
	if n != 0 {
		t.Errorf("expected 0 rows for rejected body; got %d", n)
	}
}

func TestCreateRoutingRule_ValidatesRetryOnEnum_DB(t *testing.T) {
	db := openRoutingRuleHandlerTestDB(t)
	h, _, _ := newRoutingHandlerForIT(t, db)

	// Valid: empty retryOn means "retry nothing" — accepted.
	name := uniqueName(t, "rule-rp-retryon-empty")
	rec := callCreate(t, h, `{
		"name":"`+name+`",
		"strategyType":"single",
		"config":{"providerId":"p","modelId":"m"},
		"retryPolicy":{"retryOn":[]}
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("empty retryOn rejected: %d body=%s", rec.Code, rec.Body.String())
	}
	decodeIDAndRegisterCleanup(t, db, rec)

	// Invalid: bogus class string.
	name2 := uniqueName(t, "rule-rp-retryon-bad")
	rec2 := callCreate(t, h, `{
		"name":"`+name2+`",
		"strategyType":"single",
		"config":{"providerId":"p","modelId":"m"},
		"retryPolicy":{"retryOn":["bogus","5xx"]}
	}`)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("bogus retryOn accepted: %d body=%s", rec2.Code, rec2.Body.String())
	}
}

// -- helpers --

func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }
