package alerting_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	alerting "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
)

// seedRuleRow inserts a minimal AlertRule row directly via the pool so tests
// for rule handlers have a real rule to edit.
func seedRuleRow(t *testing.T, pool *pgxpool.Pool, id, displayName string, params, paramsSchema string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO "AlertRule" (id, "displayName", "sourceType", "defaultSeverity", "requiresAck", enabled, params, "paramsSchema", "cooldownSec", "updatedAt")
		VALUES ($1, $2, 'test', 'MEDIUM'::"AlertSeverity", false, true, $3, $4, 60, NOW())
		ON CONFLICT (id) DO UPDATE SET
		  "displayName" = EXCLUDED."displayName",
		  params = EXCLUDED.params,
		  "paramsSchema" = EXCLUDED."paramsSchema"`,
		id, displayName, params, paramsSchema)
	if err != nil {
		t.Fatalf("seed rule %s: %v", id, err)
	}
}

// newAdminServer wires AdminHandlers onto a fresh Echo and returns its test server.
func newAdminServer(t *testing.T, pool *pgxpool.Pool, senders alerting.SenderRegistry, reg alerting.RuleRegistry) (*echo.Echo, *alerting.AdminHandlers) {
	t.Helper()
	store := alerting.NewStore(pool)
	h := &alerting.AdminHandlers{
		Store:   store,
		Rules:   reg,
		Senders: senders,
	}
	e := echo.New()
	e.GET("/api/v1/admin/alerts/rules", h.ListRules)
	e.GET("/api/v1/admin/alerts/rules/:id", h.GetRule)
	e.PUT("/api/v1/admin/alerts/rules/:id", h.UpdateRule)
	e.POST("/api/v1/admin/alerts/rules/:id/reset", h.ResetRule)
	e.GET("/api/v1/admin/alerts/channels", h.ListChannels)
	e.POST("/api/v1/admin/alerts/channels", h.CreateChannel)
	e.GET("/api/v1/admin/alerts/channels/:id", h.GetChannel)
	e.PUT("/api/v1/admin/alerts/channels/:id", h.UpdateChannel)
	e.DELETE("/api/v1/admin/alerts/channels/:id", h.DeleteChannel)
	e.POST("/api/v1/admin/alerts/channels/:id/test", h.ChannelTest)
	e.GET("/api/v1/admin/alerts", h.ListAlerts)
	e.GET("/api/v1/admin/alerts/:id", h.GetAlert)
	e.POST("/api/v1/admin/alerts/:id/ack", h.AckAlert)
	e.POST("/api/v1/admin/alerts/:id/resolve", h.ResolveAlert)
	return e, h
}

// adminFakeSender captures a single Send call and returns a configurable result.
type adminFakeSender struct {
	lastCh     alerting.Channel
	lastAlert  alerting.Alert
	calls      int
	statusCode int
	err        error
}

func (f *adminFakeSender) Send(_ context.Context, ch alerting.Channel, a alerting.Alert) (int, error) {
	f.lastCh = ch
	f.lastAlert = a
	f.calls++
	return f.statusCode, f.err
}

// fakeAdminSenderRegistry always returns the held sender.
type fakeAdminSenderRegistry struct {
	sender *adminFakeSender
	err    error
}

func (r *fakeAdminSenderRegistry) Get(channelType string) (alerting.Sender, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.sender, nil
}

// fakeRuleRegistry is a RuleRegistry backed by a static map.
type fakeRuleRegistry struct {
	m map[string]alerting.RuleDefault
}

func (f *fakeRuleRegistry) Lookup(id string) (alerting.RuleDefault, bool) {
	d, ok := f.m[id]
	return d, ok
}

func doJSON(t *testing.T, e *echo.Echo, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestHandleListAlerts_FilterByState(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	seedRuleRow(t, pool, "test.list.state", "List State", "{}", "{}")
	store := alerting.NewStore(pool)

	// 1 firing, 1 ack, 1 resolved.
	firingID, _ := store.InsertAlert(ctx, makeAlert("test.list.state", "test:k:firing"))
	ackID, _ := store.InsertAlert(ctx, makeAlert("test.list.state", "test:k:ack"))
	resID, _ := store.InsertAlert(ctx, makeAlert("test.list.state", "test:k:res"))
	if err := store.AcknowledgeAlert(ctx, ackID, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveAlert(ctx, resID, "alice", "done"); err != nil {
		t.Fatal(err)
	}
	_ = firingID

	e, _ := newAdminServer(t, pool, nil, nil)
	rec := doJSON(t, e, http.MethodGet, "/api/v1/admin/alerts?state=firing&ruleId=test.list.state&limit=100", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Alerts []alerting.Alert `json:"alerts"`
		Total  int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 1 {
		t.Errorf("total=%d want 1", resp.Total)
	}
	if len(resp.Alerts) != 1 {
		t.Errorf("len(alerts)=%d want 1", len(resp.Alerts))
	}
	if resp.Alerts[0].State != alerting.StateFiring {
		t.Errorf("state=%q want firing", resp.Alerts[0].State)
	}
}

// TestHandleListAlerts_MultiState verifies that repeated `state=` query
// params are all honoured by ListAlerts — the admin UI sends them as
// `state=firing&state=acknowledged` for multi-select filters.
func TestHandleListAlerts_MultiState(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	seedRuleRow(t, pool, "test.list.multi", "List Multi", "{}", "{}")
	store := alerting.NewStore(pool)

	// 1 firing, 1 ack, 1 resolved.
	_, _ = store.InsertAlert(ctx, makeAlert("test.list.multi", "test:k:mf"))
	ackID, _ := store.InsertAlert(ctx, makeAlert("test.list.multi", "test:k:ma"))
	resID, _ := store.InsertAlert(ctx, makeAlert("test.list.multi", "test:k:mr"))
	if err := store.AcknowledgeAlert(ctx, ackID, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveAlert(ctx, resID, "alice", "done"); err != nil {
		t.Fatal(err)
	}

	e, _ := newAdminServer(t, pool, nil, nil)
	rec := doJSON(t, e, http.MethodGet, "/api/v1/admin/alerts?state=firing&state=acknowledged&ruleId=test.list.multi&limit=100", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Alerts []alerting.Alert `json:"alerts"`
		Total  int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 2 {
		t.Errorf("total=%d want 2 (firing + acknowledged)", resp.Total)
	}
	if len(resp.Alerts) != 2 {
		t.Fatalf("len(alerts)=%d want 2", len(resp.Alerts))
	}
	for _, a := range resp.Alerts {
		if a.State == alerting.StateResolved {
			t.Errorf("resolved alert leaked through multi-state filter: %+v", a)
		}
	}
}

func TestHandleListAlerts_FilterBySeverity(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	seedRuleRow(t, pool, "test.list.sev", "List Sev", "{}", "{}")
	store := alerting.NewStore(pool)

	a := makeAlert("test.list.sev", "test:k:h1")
	a.Severity = alerting.SeverityHigh
	_, _ = store.InsertAlert(ctx, a)
	a2 := makeAlert("test.list.sev", "test:k:m1")
	a2.Severity = alerting.SeverityMedium
	_, _ = store.InsertAlert(ctx, a2)

	e, _ := newAdminServer(t, pool, nil, nil)
	rec := doJSON(t, e, http.MethodGet, "/api/v1/admin/alerts?severity=high&ruleId=test.list.sev", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Alerts []alerting.Alert `json:"alerts"`
		Total  int              `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Total != 1 {
		t.Errorf("total=%d want 1", resp.Total)
	}
}

func TestHandleListAlerts_Pagination(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	seedRuleRow(t, pool, "test.list.page", "List Page", "{}", "{}")
	store := alerting.NewStore(pool)

	for i := range 5 {
		a := makeAlert("test.list.page", fmt.Sprintf("test:k:p%d", i))
		// Small sleep to make firedAt strictly ordered.
		a.FiredAt = time.Now().UTC().Add(time.Duration(i) * time.Millisecond)
		a.LastSeenAt = a.FiredAt
		if _, err := store.InsertAlert(ctx, a); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	e, _ := newAdminServer(t, pool, nil, nil)
	rec := doJSON(t, e, http.MethodGet, "/api/v1/admin/alerts?ruleId=test.list.page&limit=2&offset=2", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Alerts []alerting.Alert `json:"alerts"`
		Total  int              `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Total != 5 {
		t.Errorf("total=%d want 5", resp.Total)
	}
	if len(resp.Alerts) != 2 {
		t.Errorf("len(alerts)=%d want 2", len(resp.Alerts))
	}
}

// TestHandleAlertDetail_ReturnsFlatShape pins the detail response contract:
// Alert fields live at the top level alongside `dispatches`, not nested under
// an `alert` key. The admin UI's `AlertDetailResponse extends Alert { dispatches }`
// type depends on this — the old nested shape caused every rendered field to
// come back undefined and the drawer to show empty details.
func TestHandleAlertDetail_ReturnsFlatShape(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	seedRuleRow(t, pool, "test.detail.d", "Detail", "{}", "{}")
	store := alerting.NewStore(pool)

	alertID, _ := store.InsertAlert(ctx, makeAlert("test.detail.d", "test:k:d1"))
	chID, _ := store.InsertChannel(ctx, alerting.Channel{
		Name: "test-detail-chan", Type: "webhook", Enabled: true,
		Severities: []alerting.Severity{alerting.SeverityMedium}, SourceTypes: []string{"test"},
		Config: map[string]any{"url": "http://x.example.com"},
	})
	sc := 200
	_, _ = store.InsertDispatch(ctx, alerting.Dispatch{
		AlertID: alertID, ChannelID: chID, ChannelName: "test-detail-chan",
		Success: true, StatusCode: &sc, AttemptedAt: time.Now().UTC(),
	})
	_, _ = store.InsertDispatch(ctx, alerting.Dispatch{
		AlertID: alertID, ChannelID: chID, ChannelName: "test-detail-chan",
		Success: true, StatusCode: &sc, AttemptedAt: time.Now().UTC(),
	})

	e, _ := newAdminServer(t, pool, nil, nil)
	rec := doJSON(t, e, http.MethodGet, "/api/v1/admin/alerts/"+alertID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Decode into a map so we can assert the absence of the legacy `alert` wrapper
	// key without coupling to the Go struct shape.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, nested := raw["alert"]; nested {
		t.Errorf("response contains legacy nested 'alert' key; expected flat shape")
	}
	for _, k := range []string{"id", "ruleId", "sourceType", "targetKey", "severity", "state", "message", "details", "firedAt", "dispatches"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("response missing top-level %q", k)
		}
	}

	// Also decode into the typed shape to verify values.
	var resp struct {
		alerting.Alert
		Dispatches []alerting.Dispatch `json:"dispatches"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != alertID {
		t.Errorf("id=%q want %q", resp.ID, alertID)
	}
	if resp.RuleID != "test.detail.d" {
		t.Errorf("ruleId=%q want test.detail.d", resp.RuleID)
	}
	if len(resp.Dispatches) != 2 {
		t.Errorf("len(dispatches)=%d want 2", len(resp.Dispatches))
	}
}

func TestHandleAck_SetsActorFromHeader(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	seedRuleRow(t, pool, "test.ack.hdr", "Ack Hdr", "{}", "{}")
	store := alerting.NewStore(pool)
	alertID, _ := store.InsertAlert(ctx, makeAlert("test.ack.hdr", "test:k:ack"))

	e, _ := newAdminServer(t, pool, nil, nil)
	rec := doJSON(t, e, http.MethodPost, "/api/v1/admin/alerts/"+alertID+"/ack", map[string]string{"reason": "investigating"}, map[string]string{"X-Nexus-Actor-User-Id": "alice"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	got, _ := store.GetAlert(ctx, alertID)
	if got.AcknowledgedBy == nil || *got.AcknowledgedBy != "alice" {
		t.Errorf("acknowledgedBy=%v want alice", got.AcknowledgedBy)
	}
}

func TestHandleAck_FallsBackToSystem(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	seedRuleRow(t, pool, "test.ack.sys", "Ack Sys", "{}", "{}")
	store := alerting.NewStore(pool)
	alertID, _ := store.InsertAlert(ctx, makeAlert("test.ack.sys", "test:k:ack2"))

	e, _ := newAdminServer(t, pool, nil, nil)
	rec := doJSON(t, e, http.MethodPost, "/api/v1/admin/alerts/"+alertID+"/ack", map[string]string{"reason": ""}, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	got, _ := store.GetAlert(ctx, alertID)
	if got.AcknowledgedBy == nil || *got.AcknowledgedBy != "system" {
		t.Errorf("acknowledgedBy=%v want system", got.AcknowledgedBy)
	}
}

func TestHandleResolve_SetsActorAndReason(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	seedRuleRow(t, pool, "test.res.act", "Res Act", "{}", "{}")
	store := alerting.NewStore(pool)
	alertID, _ := store.InsertAlert(ctx, makeAlert("test.res.act", "test:k:res"))

	e, _ := newAdminServer(t, pool, nil, nil)
	rec := doJSON(t, e, http.MethodPost, "/api/v1/admin/alerts/"+alertID+"/resolve", map[string]string{"reason": "fixed"}, map[string]string{"X-Nexus-Actor-User-Id": "alice"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	got, _ := store.GetAlert(ctx, alertID)
	if got.ResolvedBy == nil || *got.ResolvedBy != "alice" {
		t.Errorf("resolvedBy=%v want alice", got.ResolvedBy)
	}
	if got.ResolvedReason == nil || *got.ResolvedReason != "fixed" {
		t.Errorf("resolvedReason=%v want fixed", got.ResolvedReason)
	}
}

func TestHandleListRules_ReturnsAll(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	seedRuleRow(t, pool, "test.rules.a", "Rules A", "{}", "{}")
	seedRuleRow(t, pool, "test.rules.b", "Rules B", "{}", "{}")

	e, _ := newAdminServer(t, pool, nil, nil)
	rec := doJSON(t, e, http.MethodGet, "/api/v1/admin/alerts/rules", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Rules []alerting.AlertRule `json:"rules"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	// Assert both are present — other rules may also be seeded.
	ids := map[string]bool{}
	for _, r := range resp.Rules {
		ids[r.ID] = true
	}
	if !ids["test.rules.a"] || !ids["test.rules.b"] {
		t.Errorf("missing rules in %v", ids)
	}
}

func TestHandleGetRule_IncludesParamsSchema(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	schema := `{"type":"object","properties":{"x":{"type":"integer"}}}`
	seedRuleRow(t, pool, "test.rules.schema", "Rules Schema", `{"x":1}`, schema)

	e, _ := newAdminServer(t, pool, nil, nil)
	rec := doJSON(t, e, http.MethodGet, "/api/v1/admin/alerts/rules/test.rules.schema", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var rule alerting.AlertRule
	_ = json.Unmarshal(rec.Body.Bytes(), &rule)
	if len(rule.ParamsSchema) == 0 {
		t.Errorf("paramsSchema missing: %+v", rule.ParamsSchema)
	}
	if _, ok := rule.ParamsSchema["type"]; !ok {
		t.Errorf("paramsSchema.type missing: %+v", rule.ParamsSchema)
	}
}

func TestHandleUpdateRule_ValidParams(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	schema := `{"type":"object","properties":{"thresholds":{"type":"array","items":{"type":"integer","maximum":100,"minimum":1}}},"required":["thresholds"]}`
	seedRuleRow(t, pool, "test.rules.upd", "Rules Upd", `{"thresholds":[80,95]}`, schema)

	e, _ := newAdminServer(t, pool, nil, nil)
	newCooldown := 600
	body := map[string]any{
		"params":      map[string]any{"thresholds": []int{50, 90}},
		"cooldownSec": newCooldown,
	}
	rec := doJSON(t, e, http.MethodPut, "/api/v1/admin/alerts/rules/test.rules.upd", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var updated alerting.AlertRule
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.CooldownSec != newCooldown {
		t.Errorf("cooldownSec=%d want %d", updated.CooldownSec, newCooldown)
	}
	thresholds, ok := updated.Params["thresholds"].([]any)
	if !ok || len(thresholds) != 2 {
		t.Fatalf("params.thresholds=%v want length 2", updated.Params["thresholds"])
	}
}

func TestHandleUpdateRule_RejectsInvalidParams(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	schema := `{"type":"object","properties":{"thresholds":{"type":"array","items":{"type":"integer","maximum":100,"minimum":1}}},"required":["thresholds"]}`
	seedRuleRow(t, pool, "test.rules.bad", "Rules Bad", `{"thresholds":[80]}`, schema)

	e, _ := newAdminServer(t, pool, nil, nil)
	body := map[string]any{
		"params": map[string]any{"thresholds": []int{150}}, // max 100
	}
	rec := doJSON(t, e, http.MethodPut, "/api/v1/admin/alerts/rules/test.rules.bad", body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s want 400", rec.Code, rec.Body.String())
	}
}

func TestHandleUpdateRule_RejectsUnknownKeys(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	seedRuleRow(t, pool, "test.rules.unk", "Rules Unk", `{}`, `{"type":"object"}`)

	e, _ := newAdminServer(t, pool, nil, nil)
	body := map[string]any{"foo": "bar"}
	rec := doJSON(t, e, http.MethodPut, "/api/v1/admin/alerts/rules/test.rules.unk", body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s want 400", rec.Code, rec.Body.String())
	}
}

func TestHandleResetRule_RestoresFromRegistry(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	// Seed a rule with non-default values.
	seedRuleRow(t, pool, "test.rules.reset", "Mutated Name", `{"thresholds":[50]}`, `{"type":"object"}`)

	// Registry declares the canonical defaults for this rule id.
	reg := &fakeRuleRegistry{m: map[string]alerting.RuleDefault{
		"test.rules.reset": {
			ID:              "test.rules.reset",
			DisplayName:     "Canonical Name",
			DefaultSeverity: alerting.SeverityHigh,
			RequiresAck:     true,
			Enabled:         true,
			CooldownSec:     777,
			Params:          json.RawMessage(`{"thresholds":[80,95]}`),
			ParamsSchema:    json.RawMessage(`{"type":"object"}`),
		},
	}}

	e, _ := newAdminServer(t, pool, nil, reg)
	rec := doJSON(t, e, http.MethodPost, "/api/v1/admin/alerts/rules/test.rules.reset/reset", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	store := alerting.NewStore(pool)
	got, err := store.GetRule(context.Background(), "test.rules.reset")
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "Canonical Name" {
		t.Errorf("displayName=%q want Canonical Name", got.DisplayName)
	}
	if got.DefaultSeverity != alerting.SeverityHigh {
		t.Errorf("defaultSeverity=%q want high", got.DefaultSeverity)
	}
	if !got.RequiresAck {
		t.Errorf("requiresAck=false want true")
	}
	if got.CooldownSec != 777 {
		t.Errorf("cooldownSec=%d want 777", got.CooldownSec)
	}
	thresholds, _ := got.Params["thresholds"].([]any)
	if len(thresholds) != 2 {
		t.Errorf("params.thresholds=%v want length 2", got.Params["thresholds"])
	}
}

func TestHandleCreateChannel_MasksSecretsInResponse(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	e, _ := newAdminServer(t, pool, nil, nil)
	body := map[string]any{
		"name":        "test-create-secrets",
		"type":        "slack",
		"enabled":     true,
		"severities":  []string{"high"},
		"sourceTypes": []string{"test"},
		"config": map[string]any{
			"botToken": "xoxb-AAA-BBB-SECRETTOKEN",
			"channel":  "#alerts",
		},
	}
	rec := doJSON(t, e, http.MethodPost, "/api/v1/admin/alerts/channels", body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var ch alerting.Channel
	_ = json.Unmarshal(rec.Body.Bytes(), &ch)
	got, _ := ch.Config["botToken"].(string)
	if !strings.HasPrefix(got, "xxxx-••••-") {
		t.Errorf("botToken=%q want masked prefix", got)
	}
	if !strings.HasSuffix(got, "OKEN") {
		t.Errorf("botToken=%q want suffix OKEN", got)
	}
	// #alerts channel is not sensitive → unchanged.
	if ch.Config["channel"] != "#alerts" {
		t.Errorf("channel field altered: %v", ch.Config["channel"])
	}
}

func TestHandleListChannels_MasksSecrets(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	store := alerting.NewStore(pool)
	_, _ = store.InsertChannel(ctx, alerting.Channel{
		Name: "test-list-secret", Type: "slack", Enabled: true,
		Severities: []alerting.Severity{alerting.SeverityHigh}, SourceTypes: []string{"test"},
		Config: map[string]any{"botToken": "xoxb-SECRETVALUE"},
	})

	e, _ := newAdminServer(t, pool, nil, nil)
	rec := doJSON(t, e, http.MethodGet, "/api/v1/admin/alerts/channels", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Channels []alerting.Channel `json:"channels"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	found := false
	for _, ch := range resp.Channels {
		if ch.Name == "test-list-secret" {
			found = true
			tok, _ := ch.Config["botToken"].(string)
			if !strings.HasPrefix(tok, "xxxx-••••-") {
				t.Errorf("botToken=%q not masked", tok)
			}
		}
	}
	if !found {
		t.Errorf("channel not found in response")
	}
}

func TestHandleGetChannel_MasksSecrets(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	store := alerting.NewStore(pool)
	id, _ := store.InsertChannel(ctx, alerting.Channel{
		Name: "test-get-secret", Type: "pagerduty", Enabled: true,
		Severities: []alerting.Severity{alerting.SeverityCritical}, SourceTypes: []string{"test"},
		Config: map[string]any{"routingKey": "abcd1234"},
	})

	e, _ := newAdminServer(t, pool, nil, nil)
	rec := doJSON(t, e, http.MethodGet, "/api/v1/admin/alerts/channels/"+id, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var ch alerting.Channel
	_ = json.Unmarshal(rec.Body.Bytes(), &ch)
	rk, _ := ch.Config["routingKey"].(string)
	if !strings.HasPrefix(rk, "xxxx-••••-") {
		t.Errorf("routingKey=%q not masked", rk)
	}
	if !strings.HasSuffix(rk, "1234") {
		t.Errorf("routingKey=%q missing last 4", rk)
	}
}

// TestHandleUpdateChannel_PartialEnabledToggle pins the partial-PUT contract:
// the admin UI toggles the enabled switch by sending just {enabled: false}
// (or true). The handler must treat unspecified fields as "keep current" so
// the toggle does not require a full body. Without this, the channel list's
// quick-toggle fails with "name and type required".
func TestHandleUpdateChannel_PartialEnabledToggle(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	store := alerting.NewStore(pool)
	id, _ := store.InsertChannel(ctx, alerting.Channel{
		Name: "test-partial-toggle", Type: "webhook", Enabled: true,
		Severities: []alerting.Severity{alerting.SeverityHigh}, SourceTypes: []string{"test"},
		Config: map[string]any{"url": "http://x.example.com/hook"},
	})

	e, _ := newAdminServer(t, pool, nil, nil)
	rec := doJSON(t, e, http.MethodPut, "/api/v1/admin/alerts/channels/"+id,
		map[string]any{"enabled": false}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial PUT status=%d body=%s", rec.Code, rec.Body.String())
	}

	got, _ := store.GetChannel(ctx, id)
	if got.Enabled {
		t.Errorf("enabled=true, want false after partial PUT")
	}
	if got.Name != "test-partial-toggle" {
		t.Errorf("name=%q, want preserved", got.Name)
	}
	if got.Type != "webhook" {
		t.Errorf("type=%q, want preserved", got.Type)
	}
	if url, _ := got.Config["url"].(string); url != "http://x.example.com/hook" {
		t.Errorf("config.url=%q, want preserved", url)
	}
	if len(got.Severities) != 1 || got.Severities[0] != "high" {
		t.Errorf("severities=%v, want preserved [high]", got.Severities)
	}
}

func TestHandleUpdateChannel_PreservesSecretIfMaskedBack(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	store := alerting.NewStore(pool)
	original := "xoxb-REALREALSECRETAAAA"
	id, _ := store.InsertChannel(ctx, alerting.Channel{
		Name: "test-upd-mask", Type: "slack", Enabled: true,
		Severities: []alerting.Severity{alerting.SeverityHigh}, SourceTypes: []string{"test"},
		Config: map[string]any{"botToken": original, "channel": "#alerts"},
	})

	e, _ := newAdminServer(t, pool, nil, nil)
	// GET returns masked form.
	rec := doJSON(t, e, http.MethodGet, "/api/v1/admin/alerts/channels/"+id, nil, nil)
	var ch alerting.Channel
	_ = json.Unmarshal(rec.Body.Bytes(), &ch)
	maskedToken, _ := ch.Config["botToken"].(string)
	if !strings.HasPrefix(maskedToken, "xxxx-••••-") {
		t.Fatalf("GET did not mask: %q", maskedToken)
	}

	// PUT back the masked form unchanged.
	body := map[string]any{
		"name":        ch.Name,
		"type":        ch.Type,
		"enabled":     ch.Enabled,
		"severities":  ch.Severities,
		"sourceTypes": ch.SourceTypes,
		"config":      ch.Config,
	}
	rec = doJSON(t, e, http.MethodPut, "/api/v1/admin/alerts/channels/"+id, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}

	// DB should still have the original secret.
	raw, _ := store.GetChannel(ctx, id)
	if raw.Config["botToken"] != original {
		t.Errorf("DB botToken=%q want %q", raw.Config["botToken"], original)
	}

	// Re-GET → masked with last-4 of ORIGINAL secret (not the masked literal).
	rec = doJSON(t, e, http.MethodGet, "/api/v1/admin/alerts/channels/"+id, nil, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &ch)
	tok, _ := ch.Config["botToken"].(string)
	if !strings.HasSuffix(tok, original[len(original)-4:]) {
		t.Errorf("masked token=%q lost original suffix %q", tok, original[len(original)-4:])
	}
}

func TestHandleUpdateChannel_StoresNewSecretWhenProvided(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	ctx := context.Background()
	store := alerting.NewStore(pool)
	id, _ := store.InsertChannel(ctx, alerting.Channel{
		Name: "test-upd-new", Type: "slack", Enabled: true,
		Severities: []alerting.Severity{alerting.SeverityHigh}, SourceTypes: []string{"test"},
		Config: map[string]any{"botToken": "xoxb-OLDSECRETVALUE"},
	})

	e, _ := newAdminServer(t, pool, nil, nil)
	newSecret := "xoxb-NEWTOKENVALUE"
	body := map[string]any{
		"name":        "test-upd-new",
		"type":        "slack",
		"enabled":     true,
		"severities":  []string{"high"},
		"sourceTypes": []string{"test"},
		"config":      map[string]any{"botToken": newSecret},
	}
	rec := doJSON(t, e, http.MethodPut, "/api/v1/admin/alerts/channels/"+id, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	raw, _ := store.GetChannel(ctx, id)
	if raw.Config["botToken"] != newSecret {
		t.Errorf("DB botToken=%q want %q", raw.Config["botToken"], newSecret)
	}
}

func TestHandleDeleteChannel_404OnMissing(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)

	e, _ := newAdminServer(t, pool, nil, nil)
	rec := doJSON(t, e, http.MethodDelete, "/api/v1/admin/alerts/channels/no-such-id-"+fmt.Sprint(time.Now().UnixNano()), nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s want 404", rec.Code, rec.Body.String())
	}
}

func TestHandleChannelTest_SendsSyntheticAlertAndResolves(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)
	defer func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM "AlertDispatch" WHERE "alertId" IN (SELECT id FROM "Alert" WHERE "ruleId" = 'system.channel_test' AND "targetKey" LIKE 'channel:%')`)
		_, _ = pool.Exec(ctx, `DELETE FROM "Alert" WHERE "ruleId" = 'system.channel_test' AND "targetKey" LIKE 'channel:%'`)
	}()

	// Ensure the system.channel_test rule row exists (FK target for Alert).
	seedRuleRow(t, pool, "system.channel_test", "Channel Test", "{}", "{}")

	ctx := context.Background()
	store := alerting.NewStore(pool)
	id, _ := store.InsertChannel(ctx, alerting.Channel{
		Name: "test-chantest-ok", Type: "webhook", Enabled: true,
		Severities: []alerting.Severity{alerting.SeverityInfo}, SourceTypes: []string{"system"},
		Config: map[string]any{"url": "http://x.example.com"},
	})

	fs := &adminFakeSender{statusCode: 200, err: nil}
	reg := &fakeAdminSenderRegistry{sender: fs}

	e, _ := newAdminServer(t, pool, reg, nil)
	rec := doJSON(t, e, http.MethodPost, "/api/v1/admin/alerts/channels/"+id+"/test", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success    bool   `json:"success"`
		StatusCode int    `json:"statusCode"`
		DispatchID string `json:"dispatchId"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Success {
		t.Errorf("success=false body=%s", rec.Body.String())
	}
	if fs.calls != 1 {
		t.Errorf("sender.calls=%d want 1", fs.calls)
	}
	if fs.lastAlert.RuleID != "system.channel_test" {
		t.Errorf("sent alert ruleId=%q want system.channel_test", fs.lastAlert.RuleID)
	}

	// The synthetic alert row must exist and be resolved.
	row := pool.QueryRow(ctx, `SELECT state FROM "Alert" WHERE "targetKey" = $1 ORDER BY "firedAt" DESC LIMIT 1`, "channel:"+id)
	var state string
	if err := row.Scan(&state); err != nil {
		t.Fatalf("scan alert state: %v", err)
	}
	if !strings.EqualFold(state, "RESOLVED") {
		t.Errorf("alert state=%q want RESOLVED", state)
	}

	// Dispatch row must reference a real alert.
	var cnt int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM "AlertDispatch" WHERE "channelId" = $1`, id).Scan(&cnt)
	if cnt != 1 {
		t.Errorf("dispatch count=%d want 1", cnt)
	}
}

func TestHandleChannelTest_FailedSenderStillResolvesAlert(t *testing.T) {
	pool := testPool(t)
	defer cleanup(t, pool)
	defer func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM "AlertDispatch" WHERE "alertId" IN (SELECT id FROM "Alert" WHERE "ruleId" = 'system.channel_test' AND "targetKey" LIKE 'channel:%')`)
		_, _ = pool.Exec(ctx, `DELETE FROM "Alert" WHERE "ruleId" = 'system.channel_test' AND "targetKey" LIKE 'channel:%'`)
	}()

	seedRuleRow(t, pool, "system.channel_test", "Channel Test", "{}", "{}")

	ctx := context.Background()
	store := alerting.NewStore(pool)
	id, _ := store.InsertChannel(ctx, alerting.Channel{
		Name: "test-chantest-fail", Type: "webhook", Enabled: true,
		Severities: []alerting.Severity{alerting.SeverityInfo}, SourceTypes: []string{"system"},
		Config: map[string]any{"url": "http://x.example.com"},
	})

	fs := &adminFakeSender{statusCode: 500, err: errors.New("sender explosion")}
	reg := &fakeAdminSenderRegistry{sender: fs}

	e, _ := newAdminServer(t, pool, reg, nil)
	rec := doJSON(t, e, http.MethodPost, "/api/v1/admin/alerts/channels/"+id+"/test", nil, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s want 500", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "sender explosion") {
		t.Errorf("body=%s missing sender error", rec.Body.String())
	}

	// Alert still resolved, dispatch row records failure.
	row := pool.QueryRow(ctx, `SELECT state FROM "Alert" WHERE "targetKey" = $1 ORDER BY "firedAt" DESC LIMIT 1`, "channel:"+id)
	var state string
	_ = row.Scan(&state)
	if !strings.EqualFold(state, "RESOLVED") {
		t.Errorf("alert state=%q want RESOLVED", state)
	}
	var success bool
	_ = pool.QueryRow(ctx, `SELECT success FROM "AlertDispatch" WHERE "channelId" = $1 ORDER BY "attemptedAt" DESC LIMIT 1`, id).Scan(&success)
	if success {
		t.Errorf("dispatch.success=true want false")
	}
}

func TestMaskChannelConfig_MasksKnownKeys(t *testing.T) {
	cfg := map[string]any{
		"botToken":     "SECRETTOKEN",
		"routingKey":   "abcd1234",
		"smtpPassword": "hunter2",
		"headers": map[string]any{
			"X-Authorization": "Bearer AAABBB",
			"X-Custom":        "plain",
		},
	}
	got := alerting.MaskChannelConfig(cfg)
	if tok, _ := got["botToken"].(string); !strings.HasPrefix(tok, "xxxx-••••-") {
		t.Errorf("botToken=%q not masked", tok)
	}
	if rk, _ := got["routingKey"].(string); !strings.HasPrefix(rk, "xxxx-••••-") {
		t.Errorf("routingKey=%q not masked", rk)
	}
	if pw, _ := got["smtpPassword"].(string); !strings.HasPrefix(pw, "xxxx-••••-") {
		t.Errorf("smtpPassword=%q not masked", pw)
	}
	hdrs, _ := got["headers"].(map[string]any)
	if hv, _ := hdrs["X-Authorization"].(string); !strings.HasPrefix(hv, "xxxx-••••-") {
		t.Errorf("X-Authorization=%q not masked", hv)
	}
	if hv := hdrs["X-Custom"]; hv != "plain" {
		t.Errorf("X-Custom altered: %v", hv)
	}
}

func TestMaskChannelConfig_IgnoresNonSensitive(t *testing.T) {
	cfg := map[string]any{
		"url":     "https://hooks.slack.com/services/T00/B00/XXX",
		"channel": "#alerts",
	}
	got := alerting.MaskChannelConfig(cfg)
	if got["url"] != cfg["url"] {
		t.Errorf("url altered: %v", got["url"])
	}
	if got["channel"] != cfg["channel"] {
		t.Errorf("channel altered: %v", got["channel"])
	}
}

func TestMaskChannelConfig_MaskFormatHasLast4(t *testing.T) {
	cfg := map[string]any{"botToken": "SECRETVALUE"}
	got := alerting.MaskChannelConfig(cfg)
	tok, _ := got["botToken"].(string)
	if !strings.HasPrefix(tok, "xxxx-••••-") {
		t.Errorf("prefix wrong: %q", tok)
	}
	if !strings.HasSuffix(tok, "ALUE") {
		t.Errorf("suffix wrong: %q", tok)
	}
}

func TestMaskChannelConfig_ShortValueStillMasked(t *testing.T) {
	cfg := map[string]any{"botToken": "ab"}
	got := alerting.MaskChannelConfig(cfg)
	tok, _ := got["botToken"].(string)
	if !strings.HasPrefix(tok, "xxxx-••••-") {
		t.Errorf("prefix wrong: %q", tok)
	}
}
