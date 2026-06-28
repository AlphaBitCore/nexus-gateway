// Coverage for providers.go: RegisterProviderRoutes / ListProviders /
// GetProvider / CreateProvider (with inline models + credential) /
// UpdateProvider (3-state region/apiVersion/headers semantics) /
// DeleteProvider / ListProviderModels / AddProviderModel / GetProviderHealth /
// ListProviderCredentials. Provider CRUD is the wizard's only entry point —
// these tests pin both the happy path and every documented 4xx branch.
package providers

import (
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"
)

func TestRegisterProviderRoutes_RegistersNine(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	e := echo.New()
	g := e.Group("/api/admin")
	iamMW := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterProviderRoutes(g, iamMW)
	want := map[string]bool{
		"GET /api/admin/providers":                 false,
		"POST /api/admin/providers":                false,
		"GET /api/admin/providers/:id":             false,
		"PUT /api/admin/providers/:id":             false,
		"DELETE /api/admin/providers/:id":          false,
		"GET /api/admin/providers/:id/models":      false,
		"POST /api/admin/providers/:id/models":     false,
		"GET /api/admin/providers/:id/health":      false,
		"GET /api/admin/providers/:id/credentials": false,
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

func TestListProviders_HappyAllFilters(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Provider"`).
		WithArgs("%abc%", true).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	listRow := append(makeProviderRow(now), 5)
	mock.ExpectQuery(`SELECT p\.id, p\.name`).
		WithArgs("%abc%", true, 10, 0).
		WillReturnRows(pgxmock.NewRows(providerListCols).AddRow(listRow...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/?q=abc&enabled=true&limit=10", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.ListProviders(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestListProviders_EnabledFalseBranch(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Provider"`).
		WithArgs(false).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	listRow := append(makeProviderRow(now), 0)
	mock.ExpectQuery(`SELECT p\.id, p\.name`).
		WithArgs(false, 50, 0).
		WillReturnRows(pgxmock.NewRows(providerListCols).AddRow(listRow...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/?enabled=false", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.ListProviders(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestListProviders_StoreError500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Provider"`).
		WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.ListProviders(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestGetProvider_NotFound404(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("missing").
		WillReturnRows(pgxmock.NewRows(providerCols))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	_ = h.GetProvider(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestGetProvider_GetError500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("x").
		WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("x")
	_ = h.GetProvider(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestGetProvider_HappyWithModels(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`FROM "Model" WHERE "providerId"`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	if err := h.GetProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"models"`) || !strings.Contains(rec.Body.String(), `"headers"`) {
		t.Errorf("body must echo models + headers: %s", rec.Body.String())
	}
}

func TestGetProvider_ModelsErrorStillReturns200(t *testing.T) {
	// ListModelsByProvider error is logged but the handler still returns
	// 200 — the provider record is the primary response.
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`FROM "Model" WHERE "providerId"`).WithArgs("prov-1").
		WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	_ = h.GetProvider(c)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

// CreateProvider — validation branches

func TestCreateProvider_InvalidJSON400(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`not-json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.CreateProvider(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateProvider_MissingRequired400(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"missing name", `{"baseUrl":"https://x"}`, "name and baseUrl"},
		{"missing baseUrl", `{"name":"x"}`, "name and baseUrl"},
		{"missing adapterType", `{"name":"x","baseUrl":"https://x"}`, "adapterType is required"},
		{"invalid adapterType", `{"name":"x","baseUrl":"https://x","adapterType":"bogus"}`, "ADAPTER_TYPE_INVALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			c, _ := echoCtx(req, rec, "u-1")
			_ = h.CreateProvider(c)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d; want 400; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body must contain %q: %s", tc.want, rec.Body.String())
			}
		})
	}
}

func TestCreateProvider_InlineModelMissingFields400(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	body := `{
		"name":"alpha","baseUrl":"https://x","adapterType":"openai",
		"models":[{"providerModelId":"m1","name":"","type":"chat"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.CreateProvider(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestCreateProvider_CredentialNoVault503(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	body := `{
		"name":"alpha","baseUrl":"https://x","adapterType":"openai",
		"credential":{"name":"k","apiKey":"sk"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.CreateProvider(c)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503", rec.Code)
	}
}

func TestCreateProvider_CredentialMissingName400(t *testing.T) {
	v := newTestVault(t)
	h := newHandler(nil, nil, &auditSpy{}, nil, v, nil, ProxyConfig{})
	body := `{
		"name":"alpha","baseUrl":"https://x","adapterType":"openai",
		"credential":{"name":"","apiKey":"sk"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.CreateProvider(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

// CreateProvider — store paths

func TestCreateProvider_HappyBareProvider(t *testing.T) {
	// No models, no credential — exercises the simplest atomic create.
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectCommit()
	// incrementConfigVersion → 2 system_metadata calls.
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("agent.config.version").
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("agent.config.version", []byte("1"), "system").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	hub := &hubSpy{}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	body := `{"name":"alpha","baseUrl":"https://x","adapterType":"openai"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.CreateProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(hub.seen()) != 1 || hub.seen()[0] != "ai-gateway/providers" {
		t.Errorf("hub = %v", hub.seen())
	}
	if aud.count() != 1 {
		t.Errorf("audit count = %d; want 1", aud.count())
	}
}

func TestCreateProvider_HappyWithModelsAndCredential(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	// 1 model insert — 18 params ($1..$18 including 4 capability cols).
	mock.ExpectQuery(`INSERT INTO "Model"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	// 1 inline credential insert — note the 14-column Scan target.
	mock.ExpectQuery(`INSERT INTO "Credential"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).
			AddRow(makeCredentialRow(now)...))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	hub := &hubSpy{}
	aud := &auditSpy{}
	v := newTestVault(t)
	h := newHandler(db, hub, aud, nil, v, nil, ProxyConfig{})
	body := `{
		"name":"alpha",
		"baseUrl":"https://x",
		"adapterType":"openai",
		"region":"us-east-1",
		"apiVersion":"2024",
		"headers":{"X-Test":"1"},
		"models":[
			{"providerModelId":"m1","name":"M1","type":"chat","code":"m1-code"}
		],
		"credential":{"name":"k1","apiKey":"sk","rotationState":""}
	}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.CreateProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201; body=%s", rec.Code, rec.Body.String())
	}
	// Three hub invalidates: providers + models + credentials.
	seen := hub.seen()
	if len(seen) != 3 {
		t.Errorf("hub invalidations = %d; want 3 (%v)", len(seen), seen)
	}
}

func TestCreateProvider_HappyModelMissingCodeAndAliases(t *testing.T) {
	// Model with no code/aliases/features — defaults must populate.
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`INSERT INTO "Model"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	h := newHandler(db, &hubSpy{}, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	body := `{
		"name":"alpha","baseUrl":"https://x","adapterType":"openai",
		"models":[{"providerModelId":"m1","name":"M1","type":"chat","description":"d"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.CreateProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateProvider_NameCollision409(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23505", ConstraintName: "Provider_name_key"})
	mock.ExpectRollback()
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	body := `{"name":"alpha","baseUrl":"https://x","adapterType":"openai"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.CreateProvider(c)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "PROVIDER_NAME_EXISTS") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestCreateProvider_ModelCollision409(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`INSERT INTO "Model"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23505", ConstraintName: "Model_providerId_providerModelId_key"})
	mock.ExpectRollback()
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	body := `{
		"name":"alpha","baseUrl":"https://x","adapterType":"openai",
		"models":[{"providerModelId":"m1","name":"M1","type":"chat"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.CreateProvider(c)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "MODEL_ALREADY_REGISTERED") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestCreateProvider_GenericStoreError500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg()).
		WillReturnError(errors.New("disk full"))
	mock.ExpectRollback()
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	body := `{"name":"alpha","baseUrl":"https://x","adapterType":"openai"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.CreateProvider(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestCreateProvider_EnabledFalseAndOptionalFieldsBranch(t *testing.T) {
	// Exercises enabled=false, displayName empty (defaults to name),
	// description present, multiVault path for credential.
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`INSERT INTO "Credential"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).
			AddRow(makeCredentialRow(now)...))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mv := newTestMultiVault(t)
	h := newHandler(db, &hubSpy{}, &auditSpy{}, nil, nil, mv, ProxyConfig{})
	enabledFalse := false
	body := fmt.Sprintf(`{
		"name":"alpha","baseUrl":"https://x","adapterType":"openai",
		"description":"a description","enabled":%t,
		"credential":{"name":"k1","apiKey":"sk"}
	}`, enabledFalse)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.CreateProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201; body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpdateProvider_NotFound404(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("missing").
		WillReturnRows(pgxmock.NewRows(providerCols))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	_ = h.UpdateProvider(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestUpdateProvider_GetExistingError500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).
		WithArgs("x").WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("x")
	_ = h.UpdateProvider(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpdateProvider_InvalidBody400(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`not-json`))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	_ = h.UpdateProvider(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateProvider_InvalidAdapterType400(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"adapterType":"bogus"}`))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	_ = h.UpdateProvider(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateProvider_HappyAllFields(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`UPDATE "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	hub := &hubSpy{}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	body := `{
		"name":"renamed","displayName":"D","description":"desc",
		"baseUrl":"https://y","adapterType":"anthropic","enabled":false,
		"region":"eu-west-1","apiVersion":"2024","headers":{"X":"Y"}
	}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	if err := h.UpdateProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(hub.seen()) != 1 || hub.seen()[0] != "ai-gateway/providers" {
		t.Errorf("hub = %v", hub.seen())
	}
	if aud.count() != 1 {
		t.Errorf("audit count = %d; want 1", aud.count())
	}
}

func TestUpdateProvider_EmptyStringsTreatedAsAbsent(t *testing.T) {
	// Empty name/baseUrl/adapterType strings are converted to nil so the
	// COALESCE keeps the existing value.
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`UPDATE "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	h := newHandler(db, &hubSpy{}, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	body := `{"name":"","baseUrl":"","adapterType":"","region":null,"apiVersion":null,"headers":null}`
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	if err := h.UpdateProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestUpdateProvider_NameCollision409(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`UPDATE "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23505"})
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"name":"taken"}`))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	_ = h.UpdateProvider(c)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409", rec.Code)
	}
}

func TestUpdateProvider_GenericError500(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`UPDATE "Provider"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg()).
		WillReturnError(errors.New("disk full"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"name":"x"}`))
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	_ = h.UpdateProvider(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpdateProvider_ReadBodyError400(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", &errReader{})
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	_ = h.UpdateProvider(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeleteProvider_NotFound404(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("missing").
		WillReturnRows(pgxmock.NewRows(providerCols))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	_ = h.DeleteProvider(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestDeleteProvider_GetError500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("x").
		WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("x")
	_ = h.DeleteProvider(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestDeleteProvider_DeleteError500(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "Model"`).
		WithArgs("prov-1").WillReturnError(errors.New("fk fail"))
	mock.ExpectRollback()
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	_ = h.DeleteProvider(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestDeleteProvider_HappyAuditAndHubInvalidate(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM "Model"`).WithArgs("prov-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 5))
	mock.ExpectExec(`DELETE FROM "Provider"`).WithArgs("prov-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	hub := &hubSpy{}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	if err := h.DeleteProvider(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d; want 204", rec.Code)
	}
	if len(hub.seen()) != 1 || hub.seen()[0] != "ai-gateway/providers" {
		t.Errorf("hub = %v", hub.seen())
	}
	if aud.count() != 1 {
		t.Errorf("audit count = %d", aud.count())
	}
}

func TestListProviderModels_Happy(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Model" WHERE "providerId"`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	if err := h.ListProviderModels(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestListProviderModels_Error500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Model" WHERE "providerId"`).WithArgs("prov-1").
		WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	_ = h.ListProviderModels(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestAddProviderModel_ProviderNotFound404(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("missing").
		WillReturnRows(pgxmock.NewRows(providerCols))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	_ = h.AddProviderModel(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestAddProviderModel_ProviderLookupErrorAsNotFound(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("x").
		WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("x")
	_ = h.AddProviderModel(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 (handler collapses both branches)", rec.Code)
	}
}

func TestAddProviderModel_InvalidBody400(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`not-json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	_ = h.AddProviderModel(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestAddProviderModel_MissingNameOrType400(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":""}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	_ = h.AddProviderModel(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestAddProviderModel_Happy_DefaultsCodeAndProviderModelID(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`INSERT INTO "Model"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WillReturnRows(pgxmock.NewRows([]string{"value"}))
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	hub := &hubSpy{}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	body := `{"name":"gpt-4o","type":"chat","description":"d"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	if err := h.AddProviderModel(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d; want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(hub.seen()) != 1 || hub.seen()[0] != "ai-gateway/providers" {
		t.Errorf("hub = %v", hub.seen())
	}
	if aud.count() != 1 {
		t.Errorf("audit count = %d", aud.count())
	}
}

func TestAddProviderModel_DuplicateCollision409(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`INSERT INTO "Model"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23505"})
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	body := `{"name":"gpt-4o","type":"chat"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	_ = h.AddProviderModel(c)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409", rec.Code)
	}
}

func TestAddProviderModel_GenericError500(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Provider"\s+WHERE id`).WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows(providerCols).AddRow(makeProviderRow(now)...))
	mock.ExpectQuery(`INSERT INTO "Model"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("disk full"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	body := `{"name":"gpt-4o","type":"chat"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	_ = h.AddProviderModel(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestGetProviderHealth_StoreError500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "ProviderHealth"`).
		WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	_ = h.GetProviderHealth(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestGetProviderHealth_FoundReturnsRow(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "ProviderHealth"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"providerId", "provider", "status", "rollingErrorRate",
			"avgLatencyMs", "sampleCount", "lastRequestAt", "lastErrorAt",
		}).AddRow("prov-1", "openai", "healthy", 0.01, 120, 50, now, nil))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	if err := h.GetProviderHealth(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"healthy"`) {
		t.Errorf("body must echo health row: %s", rec.Body.String())
	}
}

func TestGetProviderHealth_NoRecordDefaultsToUnknown(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "ProviderHealth"`).
		WillReturnRows(pgxmock.NewRows([]string{
			"providerId", "provider", "status", "rollingErrorRate",
			"avgLatencyMs", "sampleCount", "lastRequestAt", "lastErrorAt",
		}))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	if err := h.GetProviderHealth(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["status"] != "unknown" {
		t.Errorf("status = %v; want unknown", resp["status"])
	}
}

func TestListProviderCredentials_StoreError500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Credential"`).
		WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	_ = h.ListProviderCredentials(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestListProviderCredentials_Happy(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Credential" c`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`SELECT c\.`).
		WithArgs("prov-1", 100, 0).
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("prov-1")
	if err := h.ListProviderCredentials(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":"cred-1"`) {
		t.Errorf("body must echo cred metadata only: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "encryptedKey") {
		t.Errorf("body must not leak encrypted fields: %s", rec.Body.String())
	}
}

// quiet linter for occasional unused fixture helpers in this file
var _ = time.Now
