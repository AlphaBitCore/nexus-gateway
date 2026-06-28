// Coverage for credential_reliability.go: ProbeCredential / UpdateCredential-
// ReliabilityOverrides / RegisterReliabilitySettingsRoutes / GetReliabilityConfig
// / UpdateReliabilityConfig. The probe path is BFF over the AI Gateway —
// tested with httptest backends; the overrides + global config paths exercise
// the credstate.Thresholds Validate cross-field invariants.
package providers

import (
	"bytes"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"
)

// --- ProbeCredential ---

func TestProbeCredential_GetError500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Credential" WHERE id`).
		WithArgs("cred-1").WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("cred-1")
	_ = h.ProbeCredential(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestProbeCredential_NotFound404(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Credential" WHERE id`).
		WithArgs("missing").
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	_ = h.ProbeCredential(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestProbeCredential_GatewayUnreachable502(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Credential" WHERE id`).WithArgs("cred-1").
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
	// Point at a host that will fail DNS / connect immediately.
	proxy := ProxyConfig{AIGatewayURL: "http://127.0.0.1:1"}
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, proxy)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"timeoutSeconds":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("cred-1")
	_ = h.ProbeCredential(c)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "AI Gateway unreachable") {
		t.Errorf("body must surface unreachable err: %s", rec.Body.String())
	}
}

func TestProbeCredential_ForwardsToGateway(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Credential" WHERE id`).WithArgs("cred-1").
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
	// Spin up a fake gateway that returns a known JSON body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/internal/v1/credentials/cred-1/probe") {
			t.Errorf("unexpected gateway path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"latencyMs":42}`))
	}))
	defer srv.Close()
	proxy := ProxyConfig{AIGatewayURL: srv.URL}
	hub := &hubSpy{}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, proxy)
	req := httptest.NewRequest(http.MethodPost, "/", nil) // empty body → defaults to {}
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("cred-1")
	if err := h.ProbeCredential(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"latencyMs":42`) {
		t.Errorf("body must echo gateway response: %s", rec.Body.String())
	}
	if aud.count() != 1 {
		t.Errorf("audit count = %d; want 1", aud.count())
	}
}

func TestProbeCredential_GatewayNonOKBodyForwarded(t *testing.T) {
	// A non-2xx body from the gateway is forwarded verbatim along with the
	// status code so the UI can render the diagnostic message.
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Credential" WHERE id`).WithArgs("cred-1").
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_api_key"}`))
	}))
	defer srv.Close()
	proxy := ProxyConfig{AIGatewayURL: srv.URL}
	aud := &auditSpy{}
	h := newHandler(db, nil, aud, nil, nil, nil, proxy)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("cred-1")
	_ = h.ProbeCredential(c)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_api_key") {
		t.Errorf("body must surface gateway error: %s", rec.Body.String())
	}
	if aud.count() != 1 {
		t.Errorf("audit must still fire on non-OK: count=%d", aud.count())
	}
}

// --- UpdateCredentialReliabilityOverrides ---

func TestUpdateOverrides_GetError500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Credential" WHERE id`).
		WithArgs("cred-1").WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("cred-1")
	_ = h.UpdateCredentialReliabilityOverrides(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpdateOverrides_NotFound404(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Credential" WHERE id`).
		WithArgs("missing").
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	_ = h.UpdateCredentialReliabilityOverrides(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestUpdateOverrides_InvalidJSON400(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Credential" WHERE id`).WithArgs("cred-1").
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"authFailThreshold":not-a-number`))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("cred-1")
	_ = h.UpdateCredentialReliabilityOverrides(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "INVALID_BODY") {
		t.Errorf("expected INVALID_BODY: %s", rec.Body.String())
	}
}

func TestUpdateOverrides_InvalidThresholdRejected(t *testing.T) {
	// degradedThresholdPct must be < healthyThresholdPct; supplying a value
	// that violates the cross-field invariant must 400.
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Credential" WHERE id`).WithArgs("cred-1").
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	body := `{"healthyThresholdPct":40,"degradedThresholdPct":60}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("cred-1")
	_ = h.UpdateCredentialReliabilityOverrides(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "INVALID_OVERRIDE") {
		t.Errorf("expected INVALID_OVERRIDE: %s", rec.Body.String())
	}
}

func TestUpdateOverrides_ClearEmptyBody(t *testing.T) {
	// Empty body / "null" / "{}" all clear the override.
	for _, body := range []string{"", "null", "{}"} {
		t.Run("body="+body, func(t *testing.T) {
			mock, db := newMockStore(t)
			now := nowFixture()
			mock.ExpectQuery(`FROM "Credential" WHERE id`).WithArgs("cred-1").
				WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
			mock.ExpectExec(`UPDATE "Credential"\s+SET    "reliabilityOverrides"`).
				WithArgs("cred-1", []byte(nil)).
				WillReturnResult(pgxmock.NewResult("UPDATE", 1))
			hub := &hubSpy{}
			aud := &auditSpy{}
			h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
			req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
			rec := httptest.NewRecorder()
			c, _ := echoCtx(req, rec, "u-1")
			c.SetParamNames("id")
			c.SetParamValues("cred-1")
			if err := h.UpdateCredentialReliabilityOverrides(c); err != nil {
				t.Fatalf("err: %v", err)
			}
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
			}
			if len(hub.seen()) != 1 || hub.seen()[0] != "ai-gateway/credentials" {
				t.Errorf("hub invalidate = %v", hub.seen())
			}
			if aud.count() != 1 {
				t.Errorf("audit count = %d; want 1", aud.count())
			}
		})
	}
}

func TestUpdateOverrides_HappyWithOverride(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Credential" WHERE id`).WithArgs("cred-1").
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
	mock.ExpectExec(`UPDATE "Credential"`).
		WithArgs("cred-1", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	hub := &hubSpy{}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	body := `{"authFailThreshold":10}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("cred-1")
	if err := h.UpdateCredentialReliabilityOverrides(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	ov, _ := resp["reliabilityOverrides"].(map[string]any)
	if ov == nil || ov["authFailThreshold"].(float64) != 10 {
		t.Errorf("override not echoed: %+v", resp)
	}
	if aud.count() != 1 {
		t.Errorf("audit count = %d; want 1", aud.count())
	}
}

func TestUpdateOverrides_SetError500(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Credential" WHERE id`).WithArgs("cred-1").
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
	mock.ExpectExec(`UPDATE "Credential"`).
		WithArgs("cred-1", pgxmock.AnyArg()).
		WillReturnError(errors.New("disk full"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"authFailThreshold":10}`))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("cred-1")
	_ = h.UpdateCredentialReliabilityOverrides(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

// --- RegisterReliabilitySettingsRoutes ---

func TestRegisterReliabilitySettingsRoutes_RegistersTwo(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	e := echo.New()
	g := e.Group("/api/admin")
	iamMW := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterReliabilitySettingsRoutes(g, iamMW)
	want := map[string]bool{
		"GET /api/admin/settings/credential-reliability": false,
		"PUT /api/admin/settings/credential-reliability": false,
	}
	for _, r := range e.Routes() {
		if _, ok := want[r.Method+" "+r.Path]; ok {
			want[r.Method+" "+r.Path] = true
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("route %s not registered", k)
		}
	}
}

// --- GetReliabilityConfig ---

func TestGetReliabilityConfig_ReadError500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("gateway.credential_reliability.config").
		WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.GetReliabilityConfig(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestGetReliabilityConfig_NoStoredValue_ReturnsDefaultsOnly(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("gateway.credential_reliability.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.GetReliabilityConfig(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, ok := resp["defaults"]; !ok {
		t.Errorf("response missing defaults: %s", rec.Body.String())
	}
}

func TestGetReliabilityConfig_StoredValueParsed(t *testing.T) {
	mock, db := newMockStore(t)
	stored := []byte(`{"authFailThreshold":7}`)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("gateway.credential_reliability.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(stored))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.GetReliabilityConfig(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	ov, _ := resp["override"].(map[string]any)
	if ov == nil || ov["authFailThreshold"].(float64) != 7 {
		t.Errorf("override not echoed: %+v", resp)
	}
}

func TestGetReliabilityConfig_StoredInvalidJSON_FallsBackToZeroOverride(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("gateway.credential_reliability.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte("not-json")))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.GetReliabilityConfig(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

// --- UpdateReliabilityConfig ---

func TestUpdateReliabilityConfig_BindError400(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.UpdateReliabilityConfig(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateReliabilityConfig_ValidateError400(t *testing.T) {
	// authFailThreshold defaults to 0 which violates the > 0 invariant.
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.UpdateReliabilityConfig(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "INVALID_THRESHOLDS") {
		t.Errorf("expected INVALID_THRESHOLDS: %s", rec.Body.String())
	}
}

func TestUpdateReliabilityConfig_SetError500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WillReturnError(errors.New("disk full"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	body := validThresholdsJSON()
	req := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.UpdateReliabilityConfig(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpdateReliabilityConfig_HappyAuditAndHubInvalidate(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("gateway.credential_reliability.config", pgxmock.AnyArg(), "u-1").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	hub := &hubSpy{}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	body := validThresholdsJSON()
	req := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.UpdateReliabilityConfig(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(hub.seen()) != 1 || hub.seen()[0] != "ai-gateway/credential_reliability" {
		t.Errorf("hub invalidates = %v", hub.seen())
	}
	if aud.count() != 1 {
		t.Errorf("audit count = %d; want 1", aud.count())
	}
}

func TestUpdateReliabilityConfig_AnonymousActorEmptyUpdatedBy(t *testing.T) {
	// Without WithAdminAuth, updatedBy must be empty — exercises the
	// nil-AdminAuth fallback branch.
	mock, db := newMockStore(t)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("gateway.credential_reliability.config", pgxmock.AnyArg(), "").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	body := validThresholdsJSON()
	req := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := anonEchoCtx(req, rec)
	if err := h.UpdateReliabilityConfig(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

// helpers shared by this file only -------------------------------------------

func validThresholdsJSON() []byte {
	t := struct {
		AuthFailThreshold              int `json:"authFailThreshold"`
		RateLimitCooldownSeconds       int `json:"rateLimitCooldownSeconds"`
		HealthyThresholdPct            int `json:"healthyThresholdPct"`
		DegradedThresholdPct           int `json:"degradedThresholdPct"`
		HealthMinSamples               int `json:"healthMinSamples"`
		HealthWindowSeconds            int `json:"healthWindowSeconds"`
		HealthSustainedDegradedSeconds int `json:"healthSustainedDegradedSeconds"`
	}{
		AuthFailThreshold: 3, RateLimitCooldownSeconds: 60,
		HealthyThresholdPct: 95, DegradedThresholdPct: 50,
		HealthMinSamples: 5, HealthWindowSeconds: 300,
		HealthSustainedDegradedSeconds: 900,
	}
	b, _ := json.Marshal(t)
	return b
}
