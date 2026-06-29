// Coverage for models.go: RegisterModelRoutes / ListModelsGrouped /
// ListModelsFlat / GetModel / UpdateModel / DeleteModel. The five admin
// model endpoints are the only authoritative path that lets operators
// mutate model pricing, deprecation, aliases, and per-model toggles.
package providers

import (
	"bytes"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

func TestRegisterModelRoutes_RegistersFive(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	e := echo.New()
	g := e.Group("/api/admin")
	iamMW := func(_ string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	h.RegisterModelRoutes(g, iamMW)
	// Five distinct (method, path) tuples we expect to be registered.
	want := map[string]bool{
		"GET /api/admin/models":        false,
		"GET /api/admin/models/flat":   false,
		"GET /api/admin/models/:id":    false,
		"PUT /api/admin/models/:id":    false,
		"DELETE /api/admin/models/:id": false,
	}
	for _, r := range e.Routes() {
		key := r.Method + " " + r.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("route %s not registered", k)
		}
	}
}

func TestListModelsGrouped_Happy(t *testing.T) {
	mock, db := newMockStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	// Provider summary query — one provider with model count 1.
	mock.ExpectQuery(`FROM "Provider" p`).
		WithArgs("prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "displayName", "description", "adapter_type", "enabled", "model_count"}).
			AddRow("prov-1", "openai", strPtr("OpenAI"), strPtr("desc"), "openai", true, 1))
	// Model query — Q + providerID both push args.
	mock.ExpectQuery(`FROM "Model"`).
		WithArgs("prov-1", "%alpha%").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))

	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/?includeEmptyProviders=true&providerId=prov-1&q=alpha", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.ListModelsGrouped(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, ok := resp["data"]; !ok {
		t.Errorf("response missing data: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestListModelsGrouped_StoreError500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Provider" p`).WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.ListModelsGrouped(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestListModelsFlat_HappyAllFilters(t *testing.T) {
	mock, db := newMockStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Model" m`).
		WithArgs("%abc%", "chat", "active", true, "prov-1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(42))
	mock.ExpectQuery(`SELECT m\..*FROM "Model"`).
		WithArgs("%abc%", "chat", "active", true, "prov-1", 25, 50).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet,
		"/?q=abc&type=chat&status=active&enabled=true&providerId=prov-1&limit=25&offset=50", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.ListModelsFlat(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["total"].(float64) != 42 {
		t.Errorf("total = %v; want 42", resp["total"])
	}
}

func TestListModelsFlat_EnabledFalseFilter(t *testing.T) {
	// enabled=false also exercises the second branch of the enabled filter.
	mock, db := newMockStore(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Model"`).
		WithArgs(false).
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`SELECT m\.`).
		WithArgs(false, 50, 0).
		WillReturnRows(pgxmock.NewRows(modelCols))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/?enabled=false", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	if err := h.ListModelsFlat(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

func TestListModelsFlat_StoreError500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM "Model"`).WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.ListModelsFlat(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestGetModel_HappyAndMissingAndError(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, db := newMockStore(t)
		now := time.Now().UTC()
		mock.ExpectQuery(`FROM "Model" WHERE id`).
			WithArgs("model-1").
			WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
		h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		c, _ := echoCtx(req, rec, "u-1")
		c.SetParamNames("id")
		c.SetParamValues("model-1")
		if err := h.GetModel(c); err != nil {
			t.Fatalf("err: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d; want 200", rec.Code)
		}
	})
	t.Run("not found 404", func(t *testing.T) {
		mock, db := newMockStore(t)
		mock.ExpectQuery(`FROM "Model" WHERE id`).
			WithArgs("missing").
			WillReturnRows(pgxmock.NewRows(modelCols))
		h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		c, _ := echoCtx(req, rec, "u-1")
		c.SetParamNames("id")
		c.SetParamValues("missing")
		_ = h.GetModel(c)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d; want 404", rec.Code)
		}
	})
	t.Run("db error 500", func(t *testing.T) {
		mock, db := newMockStore(t)
		mock.ExpectQuery(`FROM "Model" WHERE id`).
			WithArgs("x").
			WillReturnError(errors.New("planner err"))
		h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		c, _ := echoCtx(req, rec, "u-1")
		c.SetParamNames("id")
		c.SetParamValues("x")
		_ = h.GetModel(c)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d; want 500", rec.Code)
		}
	})
}

// updateModelReq is the inline body type for UpdateModel test cases.
func putReq(t *testing.T, body any, id string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues(id)
	return c, rec
}

func TestUpdateModel_ExistingNotFound404(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("missing").
		WillReturnRows(pgxmock.NewRows(modelCols))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	c, rec := putReq(t, map[string]any{"name": "x"}, "missing")
	_ = h.UpdateModel(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestUpdateModel_GetExistingError500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Model" WHERE id`).
		WithArgs("x").WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	c, rec := putReq(t, map[string]any{}, "x")
	_ = h.UpdateModel(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestUpdateModel_InvalidBody400(t *testing.T) {
	mock, db := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("model-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("model-1")
	_ = h.UpdateModel(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestUpdateModel_InvalidTypeRejected(t *testing.T) {
	mock, db := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("model-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	c, rec := putReq(t, map[string]any{"type": "bogus"}, "model-1")
	_ = h.UpdateModel(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "chat, embedding") {
		t.Errorf("error must enumerate allowed types: %s", rec.Body.String())
	}
}

func TestUpdateModel_EmptyCodeRejected(t *testing.T) {
	mock, db := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("model-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	c, rec := putReq(t, map[string]any{"code": ""}, "model-1")
	_ = h.UpdateModel(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `code must not be empty`) {
		t.Errorf("expected code-empty msg: %s", rec.Body.String())
	}
}

func TestUpdateModel_EmptyProviderModelIDRejected(t *testing.T) {
	mock, db := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("model-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	c, rec := putReq(t, map[string]any{"providerModelId": ""}, "model-1")
	_ = h.UpdateModel(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `providerModelId must not be empty`) {
		t.Errorf("expected providerModelId-empty msg: %s", rec.Body.String())
	}
}

func TestUpdateModel_HappyAuditAndHubInvalidate(t *testing.T) {
	mock, db := newMockStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("model-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	// UpdateModel store call → return updated row. 22 positional args
	// ($1=id, $2...$22 = 21 COALESCE/CASE params; D-6 added 2 cached price cols).
	mock.ExpectQuery(`UPDATE "Model"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	hub := &hubSpy{}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	c, rec := putReq(t, map[string]any{
		"type": "chat", "name": "Updated",
		"aliases":  []string{"a", "b"},
		"features": []string{"vision"},
	}, "model-1")
	if err := h.UpdateModel(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(hub.seen()) != 1 || hub.seen()[0] != "ai-gateway/models" {
		t.Errorf("hub invalidations = %v; want one ai-gateway/models", hub.seen())
	}
	if aud.count() != 1 {
		t.Errorf("audit count = %d; want 1", aud.count())
	}
	// Confirm the audit Action matches the iam Verb.
	m := aud.last()
	if m["action"] != string(iam.VerbUpdate) {
		t.Errorf("audit action = %v; want %v", m["action"], iam.VerbUpdate)
	}
}

func TestUpdateModel_InvalidCapabilityJsonRejected(t *testing.T) {
	mock, db := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("model-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	c, rec := putReq(t, map[string]any{"capabilityJson": "not-a-json-object"}, "model-1")
	_ = h.UpdateModel(c)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 for malformed capabilityJson", rec.Code)
	}
}

func TestUpdateModel_ValidCapabilityJsonAccepted(t *testing.T) {
	mock, db := newMockStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("model-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	mock.ExpectQuery(`UPDATE "Model"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	c, rec := putReq(t, map[string]any{
		"capabilityJson": map[string]any{
			"embeddings": map[string]any{
				"supported_dimensions": []int{256, 512, 1536},
				"max_batch_size":       96,
			},
		},
	}, "model-1")
	if err := h.UpdateModel(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 for valid capabilityJson; body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpdateModel_UpdateStoreError500(t *testing.T) {
	mock, db := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("model-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	mock.ExpectQuery(`UPDATE "Model"`).WillReturnError(errors.New("constraint"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	c, rec := putReq(t, map[string]any{"name": "X"}, "model-1")
	_ = h.UpdateModel(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestDeleteModel_NotFound404(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("missing").
		WillReturnRows(pgxmock.NewRows(modelCols))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("missing")
	_ = h.DeleteModel(c)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestDeleteModel_GetExistingError500(t *testing.T) {
	mock, db := newMockStore(t)
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("x").
		WillReturnError(errors.New("planner err"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("x")
	_ = h.DeleteModel(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestDeleteModel_DeleteError500(t *testing.T) {
	mock, db := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("model-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	mock.ExpectExec(`DELETE FROM "Model" WHERE id`).
		WithArgs("model-1").WillReturnError(errors.New("fk constraint"))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("model-1")
	_ = h.DeleteModel(c)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

func TestDeleteModel_HappyAuditAndHubInvalidate(t *testing.T) {
	mock, db := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM "Model" WHERE id`).WithArgs("model-1").
		WillReturnRows(pgxmock.NewRows(modelCols).AddRow(makeModelRow(now)...))
	mock.ExpectExec(`DELETE FROM "Model" WHERE id`).
		WithArgs("model-1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	hub := &hubSpy{}
	aud := &auditSpy{}
	h := newHandler(db, hub, aud, nil, nil, nil, ProxyConfig{})
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("model-1")
	if err := h.DeleteModel(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d; want 204", rec.Code)
	}
	if len(hub.seen()) != 1 || hub.seen()[0] != "ai-gateway/models" {
		t.Errorf("hub invalidations = %v", hub.seen())
	}
	if aud.count() != 1 {
		t.Errorf("audit count = %d; want 1", aud.count())
	}
	m := aud.last()
	if m["action"] != string(iam.VerbDelete) {
		t.Errorf("audit action = %v; want %v", m["action"], iam.VerbDelete)
	}
}

// Empty body Content-Type so c.Bind doesn't choke on optional fields.
var _ = errors.New
